package jsonrpc

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/log"

	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/types"
	"github.com/0xPolygonHermez/zkevm-node/pool"
	"github.com/0xPolygonHermez/zkevm-node/sequencer"
	"github.com/0xPolygonHermez/zkevm-node/state"
	stateMetrics "github.com/0xPolygonHermez/zkevm-node/state/metrics"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime/executor"
	"github.com/ethereum/go-ethereum/common"
	ethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v4"
)

// SimulatorEndpoints is the simulator jsonrpc endpoint
type SimulatorEndpoints struct{
	cfg      Config
	pool     types.PoolInterface
	state    types.StateInterface
	etherman types.EthermanInterface
	txMan    DBTxManager

	sequencerAddr common.Address

	batchNumber   uint64
	imStateRoot common.Hash
}

type simulationResponse struct {
	Results []bool `json:"results"`
}

// L2Block represents a wip or processed L2 block
type L2Block struct {
	createdAt                 time.Time
	trackingNum               uint64
	timestamp                 uint64
	deltaTimestamp            uint32
	imStateRoot               common.Hash
	l1InfoTreeExitRoot        state.L1InfoTreeExitRootStorageEntry
	l1InfoTreeExitRootChanged bool
	bytes                     uint64
	usedZKCounters            state.ZKCounters
	reservedZKCounters        state.ZKCounters
	transactions              []*sequencer.TxTracker
	batchResponse             *state.ProcessBatchResponse
}

// NewSimulatorEndpoints creates an new instance of the simulation
func NewSimulatorEndpoints(cfg Config, pool types.PoolInterface, state types.StateInterface, etherman types.EthermanInterface) *SimulatorEndpoints {
	e := &SimulatorEndpoints{
		cfg: cfg, 
		pool: pool, 
		state: state, 
		etherman: etherman, 
	}

	sequencerAddr, err := etherman.TrustedSequencer()
	if err != nil {
		fmt.Errorf("failed to get trusted sequencer address, error: %v", err)
	}

	e.sequencerAddr = sequencerAddr

	fmt.Println("stompesi - sequencerAddr", sequencerAddr)

	return e
}

func (s *SimulatorEndpoints) SimulateBlock(RawTxList []string) (interface{}, types.Error) {
	return s.txMan.NewDbTxScope(s.state, func(ctx context.Context, dbTx pgx.Tx) (interface{}, types.Error) {

		lastL2Block, err := s.state.GetLastL2Block(ctx, nil)
		if err != nil {
			log.Fatalf("failed to get last L2 block number, error: %v", err)
			
			return RPCErrorResponse(types.DefaultErrorCode, "failed to get last L2 block number", err, true)
		}

		lastBatchNum, err := s.state.GetLastBatchNumber(ctx, dbTx)
		if err != nil {
			log.Fatalf("failed to get last batch number, error: %v", err)

			return RPCErrorResponse(types.DefaultErrorCode, "failed to get last batch number", err, true)
		}

		// Get the last batch in trusted state
		lastStateBatch, err := s.state.GetBatchByNumber(ctx, lastBatchNum, dbTx)
		if err != nil {
			log.Fatalf("failed to get last batch %d, error: %v", lastBatchNum, err)

			return RPCErrorResponse(types.DefaultErrorCode, "failed to get last batch", err, true)
		}

		newL2Block := s.generateL2Block(ctx, uint64(lastL2Block.ReceivedAt.Unix()))

		isClosed := !lastStateBatch.WIP

		fmt.Println("batch %d isClosed: %v", lastBatchNum, isClosed)

		// s.batchNumber = lastStateBatch.BatchNumber
		// s.initialStateRoot = lastStateBatch.StateRoot
		// s.imStateRoot = lastStateBatch.StateRoot
		// s.finalStateRoot = lastStateBatch.StateRoot
		
		for _, txString := range RawTxList {
				tx, _ := hexToTx(txString)
				poolTx := pool.NewTransaction(*tx, "", false)
				
				// processBatchResponse, _ := s.state.PreProcessTransaction(ctx, tx, dbTx)
				// poolTx.ZKCounters = processBatchResponse.UsedZkCounters
				// poolTx.ReservedZKCounters = processBatchResponse.ReservedZkCounters

				txTracker, _ := newTxTracker(poolTx.Transaction, poolTx.ZKCounters, poolTx.ReservedZKCounters, poolTx.IP)

				newL2Block.transactions = append(newL2Block.transactions, txTracker)
		}
		
		batchResponse, _, err := s.executeL2Block(ctx, lastStateBatch.StateRoot, newL2Block)
		
		if err != nil {
			log.Fatalf("failed to execute L2 block [%d], error: %v", newL2Block.trackingNum, err)

			return RPCErrorResponse(types.DefaultErrorCode, "failed to execute L2 block", err, true)
		}

		if len(batchResponse.BlockResponses) != 1 {
			log.Fatalf("length of batchResponse.BlockRespones returned by the executor is %d and must be 1", len(batchResponse.BlockResponses))

			return RPCErrorResponse(types.DefaultErrorCode, "length of batchResponse.BlockRespones must be 1", err, true)
		}

		blockResponse := batchResponse.BlockResponses[0]

		// Sanity check. Check blockResponse.TransactionsReponses match l2Block.Transactions length, order and tx hashes
		if len(blockResponse.TransactionResponses) != len(newL2Block.transactions) {
			log.Fatalf("length of TransactionsResponses %d doesn't match length of l2Block.transactions %d", len(blockResponse.TransactionResponses), len(newL2Block.transactions))

			return RPCErrorResponse(types.DefaultErrorCode, "length of TransactionsResponses doesn't match length of l2Block", nil, false)
		}
		
		simulatedResult := []uint32{}
		
		for i, txResponse := range blockResponse.TransactionResponses {
			if txResponse.TxHash != newL2Block.transactions[i].Hash {
				log.Fatalf("blockResponse.TransactionsResponses[%d] hash %s doesn't match l2Block.transactions[%d] hash %s", i, txResponse.TxHash.String(), i, newL2Block.transactions[i].Hash)

				return RPCErrorResponse(types.DefaultErrorCode, "blockResponse.TransactionsResponses hash doesn't match l2Block.transactions hash", nil, false)
			}

			if txResponse.RomError != nil {
				simulatedResult = append(simulatedResult, 0)
			} else {
				simulatedResult = append(simulatedResult, 1)
			}

			fmt.Println("stompesi - txResponse", txResponse)
			fmt.Println("stompesi - txResponse.RomError", txResponse.RomError)
			fmt.Println("stompesi - txResponse.ReturnValue", txResponse.ReturnValue)
			fmt.Println("")
		}

		return simulatedResult, nil
	})
}

func (s *SimulatorEndpoints) SimulateTransactions(RawTxList []string) (interface{}, types.Error) {
	return s.txMan.NewDbTxScope(s.state, func(ctx context.Context, dbTx pgx.Tx) (interface{}, types.Error) {
		lastBatchNum, err := s.state.GetLastBatchNumber(ctx, dbTx)
		if err != nil {
			log.Fatalf("failed to get last batch number, error: %v", err)
		}

		// Get the last batch in trusted state
		lastStateBatch, err := s.state.GetBatchByNumber(ctx, lastBatchNum, dbTx)
		if err != nil {
			log.Fatalf("failed to get last batch %d, error: %v", lastBatchNum, err)
		}

		s.batchNumber = lastStateBatch.BatchNumber
		s.imStateRoot = lastStateBatch.StateRoot
		
		simulatedResult := []uint32{}

		for _, txString := range RawTxList {
				tx, _ := hexToTx(txString)
				
				processBatchResponse, _ := s.state.PreProcessTransaction(ctx, tx, dbTx)
				poolTx := pool.NewTransaction(*tx, "", false)
				poolTx.ZKCounters = processBatchResponse.UsedZkCounters
				poolTx.ReservedZKCounters = processBatchResponse.ReservedZkCounters

				txTracker, _ := newTxTracker(poolTx.Transaction, poolTx.ZKCounters, poolTx.ReservedZKCounters, poolTx.IP)

				result := s.processTransaction(ctx, txTracker)

				if result {
					simulatedResult = append(simulatedResult, 1)
					} else {
					simulatedResult = append(simulatedResult, 0)
				}
		}
	
		return simulatedResult, nil
	})
}

// processTransaction processes a single transaction.
func (s *SimulatorEndpoints) processTransaction(ctx context.Context, tx *sequencer.TxTracker) bool {
	batchRequest := state.ProcessRequest{
		BatchNumber:               s.batchNumber,
		OldStateRoot:              s.imStateRoot,
		Coinbase:                  s.sequencerAddr,
		L1InfoRoot_V2:             state.GetMockL1InfoRoot(),
		TimestampLimit_V2:         uint64(time.Now().Unix()),
		Caller:                    stateMetrics.DiscardCallerLabel,
		ForkID:                    s.state.GetForkIDByBatchNumber(s.batchNumber),
		Transactions:              tx.RawTx,
		SkipFirstChangeL2Block_V2: true,
		SkipWriteBlockInfoRoot_V2: true,
		SkipVerifyL1InfoRoot_V2:   true,
		L1InfoTreeData_V2:         map[uint32]state.L1DataV2{},
		ExecutionMode:             executor.ExecutionMode0,
	}

	// Assign applied EGP percentage to tx (TxTracker)
	tx.EGPPercentage = state.MaxEffectivePercentage
	effectivePercentageAsDecodedHex, err := hex.DecodeHex(fmt.Sprintf("%x", tx.EGPPercentage))
	if err != nil {
		return false
	}

	batchRequest.Transactions = append(batchRequest.Transactions, effectivePercentageAsDecodedHex...)
	batchResponse, err := s.state.ProcessBatchV2(ctx, batchRequest, false)

	fmt.Println("stompesi - batchResponse", batchResponse)
	fmt.Println("stompesi - err", err)

	if err != nil && (errors.Is(err, runtime.ErrExecutorDBError) || errors.Is(err, runtime.ErrInvalidTxChangeL2BlockMinTimestamp)) {
		log.Errorf("failed to process tx %s, error: %v", tx.HashStr, err)
		return false
	} else if err != nil {
		log.Errorf("error received from executor, error: %v", err)
		return false
	}

	blockResponse := batchResponse.BlockResponses[0]

	fmt.Println("processTransaction - stompesi - blockResponse.TransactionResponses", blockResponse.TransactionResponses)
	
	for _, txResponse := range blockResponse.TransactionResponses {
		fmt.Println("processTransaction - stompesi - txResponse", txResponse)
		fmt.Println("processTransaction - stompesi - txResponse.RomError", txResponse.RomError)
		fmt.Println("processTransaction - stompesi - txResponse.ReturnValue", txResponse.ReturnValue)
		fmt.Println("")

		if txResponse.RomError != nil {
			return false
		} 
	}

	// Update imStateRoot
	s.imStateRoot = batchResponse.NewStateRoot

	return true
}

func (s *SimulatorEndpoints) Simulate() (interface{}, types.Error) {
	return s.txMan.NewDbTxScope(s.state, func(ctx context.Context, dbTx pgx.Tx) (interface{}, types.Error) {
		resp := simulationResponse{
			Results: []bool{},
		}

		return resp, nil
	})
}

// executeL2Block executes a L2 Block in the executor and returns the batch response from the executor and the batchL2Data size
func (s *SimulatorEndpoints) executeL2Block(ctx context.Context, initialStateRoot common.Hash, l2Block *L2Block) (*state.ProcessBatchResponse, uint64, error) {
	executeL2BLockError := func(err error) {
		log.Errorf("execute L2 block [%d] error %v, batch: %d, initialStateRoot: %s", l2Block.trackingNum, err, s.batchNumber, initialStateRoot)
		// Log batch detailed info
		for i, tx := range l2Block.transactions {
			log.Infof("batch: %d, block: [%d], tx position: %d, tx hash: %s", s.batchNumber, l2Block.trackingNum, i, tx.HashStr)
		}
	}

	batchL2Data := []byte{}

	// Add changeL2Block to batchL2Data
	changeL2BlockBytes := s.state.BuildChangeL2Block(l2Block.deltaTimestamp, l2Block.getL1InfoTreeIndex())
	batchL2Data = append(batchL2Data, changeL2BlockBytes...)

	// Add transactions data to batchL2Data
	for _, tx := range l2Block.transactions {
		epHex, err := hex.DecodeHex(fmt.Sprintf("%x", tx.EGPPercentage))
		if err != nil {
			log.Errorf("error decoding hex value for effective gas price percentage for tx %s, error: %v", tx.HashStr, err)
			return nil, 0, err
		}

		txData := append(tx.RawTx, epHex...)

		batchL2Data = append(batchL2Data, txData...)
	}

	batchRequest := state.ProcessRequest{
		BatchNumber:               s.batchNumber,
		OldStateRoot:              initialStateRoot,
		Coinbase:                  s.sequencerAddr,
		L1InfoRoot_V2:             state.GetMockL1InfoRoot(),
		TimestampLimit_V2:         l2Block.timestamp,
		Transactions:              batchL2Data,
		SkipFirstChangeL2Block_V2: false,
		SkipWriteBlockInfoRoot_V2: false,
		Caller:                    stateMetrics.DiscardCallerLabel,
		ForkID:                    s.state.GetForkIDByBatchNumber(s.batchNumber),
		SkipVerifyL1InfoRoot_V2:   true,
		L1InfoTreeData_V2:         map[uint32]state.L1DataV2{},
		ExecutionMode:             executor.ExecutionMode0,
	}

	batchRequest.L1InfoTreeData_V2[l2Block.l1InfoTreeExitRoot.L1InfoTreeIndex] = state.L1DataV2{
		GlobalExitRoot: l2Block.l1InfoTreeExitRoot.GlobalExitRoot.GlobalExitRoot,
		BlockHashL1:    l2Block.l1InfoTreeExitRoot.PreviousBlockHash,
		MinTimestamp:   uint64(l2Block.l1InfoTreeExitRoot.GlobalExitRoot.Timestamp.Unix()),
	}

	var (
		err           error
		batchResponse *state.ProcessBatchResponse
	)

	batchResponse, err = s.state.ProcessBatchV2(ctx, batchRequest, true)
	

	if err != nil {
		executeL2BLockError(err)
		return nil, 0, err
	}

	if batchResponse.ExecutorError != nil {
		executeL2BLockError(batchResponse.ExecutorError)
		return nil, 0, errors.New("executor error")
	}

	if batchResponse.IsRomOOCError {
		executeL2BLockError(batchResponse.RomError_V2)
		return nil, 0, errors.New("processing batch OOC")
	}

	return batchResponse, uint64(len(batchL2Data)), nil
}

// newTxTracker creates and inti a TxTracker
func newTxTracker(tx ethTypes.Transaction, usedZKCounters state.ZKCounters, reservedZKCounters state.ZKCounters, ip string) (*sequencer.TxTracker, error) {
	addr, err := state.GetSender(tx)
	if err != nil {
		return nil, err
	}

	rawTx, err := state.EncodeTransactionWithoutEffectivePercentage(tx)
	if err != nil {
		return nil, err
	}

	txTracker := &sequencer.TxTracker{
		Hash:               tx.Hash(),
		HashStr:            tx.Hash().String(),
		From:               addr,
		FromStr:            addr.String(),
		Nonce:              tx.Nonce(),
		Gas:                tx.Gas(),
		GasPrice:           tx.GasPrice(),
		Cost:               tx.Cost(),
		Bytes:              uint64(len(rawTx)) + state.EfficiencyPercentageByteLength,
		UsedZKCounters:     usedZKCounters,
		ReservedZKCounters: reservedZKCounters,
		RawTx:              rawTx,
		ReceivedAt:         time.Now(),
		IP:                 ip,
		EffectiveGasPrice:  new(big.Int).SetUint64(0),
		EGPLog: state.EffectiveGasPriceLog{
			ValueFinal:     new(big.Int).SetUint64(0),
			ValueFirst:     new(big.Int).SetUint64(0),
			ValueSecond:    new(big.Int).SetUint64(0),
			FinalDeviation: new(big.Int).SetUint64(0),
			MaxDeviation:   new(big.Int).SetUint64(0),
			GasPrice:       new(big.Int).SetUint64(0),
		},
	}

	return txTracker, nil
}

func (s *SimulatorEndpoints) generateL2Block(ctx context.Context, prevTimestamp uint64) *L2Block {
	lastL1BlockNumber, err := s.etherman.GetLatestBlockNumber(ctx)
	if err != nil {
		log.Errorf("error getting latest L1 block number, error: %v", err)
		return nil
	}

	l1InfoRoot, err := s.state.GetLatestL1InfoRoot(ctx, lastL1BlockNumber)
	if err != nil {
		log.Errorf("error checking latest L1InfoRoot, error: %v", err)
		return nil
	}

	newL2Block := &L2Block{}
	newL2Block.createdAt = time.Now()

	// Tracking number
	newL2Block.trackingNum = 0

	newL2Block.deltaTimestamp = uint32(uint64(time.Now().Unix()) - prevTimestamp)
	newL2Block.timestamp = prevTimestamp + uint64(newL2Block.deltaTimestamp)

	newL2Block.transactions = []*sequencer.TxTracker{}
	newL2Block.l1InfoTreeExitRoot = l1InfoRoot
	
	lastGER, err := s.state.GetLatestBatchGlobalExitRoot(ctx, nil)
	if err == nil {
		newL2Block.l1InfoTreeExitRootChanged = (newL2Block.l1InfoTreeExitRoot.GlobalExitRoot.GlobalExitRoot != lastGER)
	} else {
		// If we got an error when getting the latest GER then we consider that the index has not changed and it will be updated the next time we have a new L1InfoTreeIndex
		log.Warnf("failed to get the latest CER when initializing the WIP L2 block, assuming L1InfoTreeIndex has not changed, error: %v", err)
	}

	return newL2Block
}

// getL1InfoTreeIndex returns the L1InfoTreeIndex that must be used when processing/storing the block
func (b *L2Block) getL1InfoTreeIndex() uint32 {
	// If the L1InfoTreeIndex has changed in this block then we return the new index, otherwise we return 0
	if b.l1InfoTreeExitRootChanged {
		return b.l1InfoTreeExitRoot.L1InfoTreeIndex
	} else {
		return 0
	}
}
