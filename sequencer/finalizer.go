package sequencer

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/sbbclient"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/iden3/go-iden3-crypto/keccak256"

	"github.com/0xPolygonHermez/zkevm-data-streamer/datastreamer"
	"github.com/0xPolygonHermez/zkevm-node/event"
	"github.com/0xPolygonHermez/zkevm-node/hex"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/pool"
	"github.com/0xPolygonHermez/zkevm-node/state"
	stateMetrics "github.com/0xPolygonHermez/zkevm-node/state/metrics"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime/executor"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	pendingL2BlocksBufferSize = 100
	changeL2BlockSize         = 9 //1 byte (tx type = 0B) + 4 bytes for deltaTimestamp + 4 for l1InfoTreeIndex
)

var (
	now = time.Now
)

type Method string

const (
	FinalizeBlock          Method = "finalize_block"
	GetRawTransactionList  Method = "get_raw_transaction_list"
	GetSequencerRpcUrlList Method = "get_sequencer_rpc_url_list"
)

// finalizer represents the finalizer component of the sequencer.
type finalizer struct {
	cfg              FinalizerCfg
	isSynced         func(ctx context.Context) bool
	sequencerAddress common.Address
	workerIntf       workerInterface
	poolIntf         txPool
	stateIntf        stateInterface
	etherman         etherman
	wipBatch         *Batch
	wipL2Block       *L2Block
	batchConstraints state.BatchConstraintsCfg
	haltFinalizer    atomic.Bool
	// forced batches
	nextForcedBatches       []state.ForcedBatch
	nextForcedBatchDeadline int64
	nextForcedBatchesMux    *sync.Mutex
	lastForcedBatchNum      uint64
	// L1InfoTree
	lastL1InfoTreeValid bool
	lastL1InfoTree      state.L1InfoTreeExitRootStorageEntry
	lastL1InfoTreeMux   *sync.Mutex
	lastL1InfoTreeCond  *sync.Cond
	// event log
	eventLog *event.EventLog
	// effective gas price calculation instance
	effectiveGasPrice *pool.EffectiveGasPrice
	// pending L2 blocks to process (executor)
	pendingL2BlocksToProcess   chan *L2Block
	pendingL2BlocksToProcessWG *sync.WaitGroup
	// pending L2 blocks to store in the state
	pendingL2BlocksToStore   chan *L2Block
	pendingL2BlocksToStoreWG *sync.WaitGroup
	// L2 block counter for tracking purposes
	l2BlockCounter uint64
	// executor flushid control
	proverID           string
	storedFlushID      uint64
	storedFlushIDCond  *sync.Cond //Condition to wait until storedFlushID has been updated
	lastPendingFlushID uint64
	pendingFlushIDCond *sync.Cond
	// worker ready txs condition
	workerReadyTxsCond *timeoutCond
	// interval metrics
	metrics *intervalMetrics
	// stream server
	streamServer *datastreamer.StreamServer
	dataToStream chan interface{}

	// sbb
	ethClient *ethclient.Client
	sbbClient *sbbclient.SbbClient

	sequencerRpcUrls     []string
	sequencerAddresses   []string
	leaderSequencerIndex uint64

	preparedTxsBlockNumber    uint64
	
	sequencerPrivateKey *ecdsa.PrivateKey

	blockTransactions map[uint64]types.Transactions
}

// newFinalizer returns a new instance of Finalizer.
func newFinalizer(
	cfg FinalizerCfg,
	poolCfg pool.Config,
	workerIntf workerInterface,
	poolIntf txPool,
	stateIntf stateInterface,
	etherman etherman,
	sequencerAddr common.Address,
	isSynced func(ctx context.Context) bool,
	batchConstraints state.BatchConstraintsCfg,
	eventLog *event.EventLog,
	streamServer *datastreamer.StreamServer,
	workerReadyTxsCond *timeoutCond,
	dataToStream chan interface{},
	sequencerPrivateKey *ecdsa.PrivateKey,
) *finalizer {
	sbbClient := sbbclient.New()
	ethClient, _ := ethclient.Dial(cfg.PlatformUrl) // TODO: error handling

	f := finalizer{
		cfg:              cfg,
		isSynced:         isSynced,
		sequencerAddress: sequencerAddr,
		workerIntf:       workerIntf,
		poolIntf:         poolIntf,
		stateIntf:        stateIntf,
		etherman:         etherman,
		batchConstraints: batchConstraints,
		// forced batches
		nextForcedBatches:       make([]state.ForcedBatch, 0),
		nextForcedBatchDeadline: 0,
		nextForcedBatchesMux:    new(sync.Mutex),
		// L1InfoTree
		lastL1InfoTreeValid: false,
		lastL1InfoTreeMux:   new(sync.Mutex),
		lastL1InfoTreeCond:  sync.NewCond(&sync.Mutex{}),
		// event log
		eventLog: eventLog,
		// effective gas price calculation instance
		effectiveGasPrice: pool.NewEffectiveGasPrice(poolCfg.EffectiveGasPrice),
		// pending L2 blocks to process (executor)
		pendingL2BlocksToProcess:   make(chan *L2Block, pendingL2BlocksBufferSize),
		pendingL2BlocksToProcessWG: new(sync.WaitGroup),
		// pending L2 blocks to store in the state
		pendingL2BlocksToStore:   make(chan *L2Block, pendingL2BlocksBufferSize),
		pendingL2BlocksToStoreWG: new(sync.WaitGroup),
		storedFlushID:            0,
		// executor flushid control
		proverID:           "",
		storedFlushIDCond:  sync.NewCond(&sync.Mutex{}),
		lastPendingFlushID: 0,
		pendingFlushIDCond: sync.NewCond(&sync.Mutex{}),
		// worker ready txs condition
		workerReadyTxsCond: workerReadyTxsCond,
		// metrics
		metrics: newIntervalMetrics(cfg.Metrics.Interval.Duration),
		// stream server
		streamServer: streamServer,
		dataToStream: dataToStream,

		// sbb
		sbbClient:                 sbbClient,
		ethClient:                 ethClient,
		sequencerRpcUrls:          make([]string, 0),
		sequencerAddresses:        make([]string, 0),
		leaderSequencerIndex:      0,
		preparedTxsBlockNumber:    0, // TODO: check if this is correct

		sequencerPrivateKey: sequencerPrivateKey,

		blockTransactions: make(map[uint64]types.Transactions),
	}
	f.haltFinalizer.Store(false)
	return &f
}

// Start starts the finalizer.
func (f *finalizer) Start(ctx context.Context) {
	// Do sanity check for batches closed but pending to be checked
	f.processBatchesPendingtoCheck(ctx)

	// Update L1InfoRoot
	go f.checkL1InfoTreeUpdate(ctx)

	// Get the last batch if still wip or opens a new one
	f.initWIPBatch(ctx)

	// Initializes the wip L2 block
	f.initWIPL2Block(ctx)

	// Update the prover id and flush id
	go f.updateProverIdAndFlushId(ctx)

	// Process L2 Blocks
	go f.processPendingL2Blocks(ctx)

	// Store L2 Blocks
	go f.storePendingL2Blocks(ctx)

	// Foced batches checking
	go f.checkForcedBatches(ctx)

	
	if f.cfg.UseExternalSequencer {	
		lastL2Block, err := f.stateIntf.GetLastL2Block(ctx, nil)
		if err != nil {
			log.Fatalf("failed to get last L2 block number, error: %v", err)
		}
		finalizeBlockNumber := lastL2Block.Number().Uint64()

		go f.requestFinalizeBlockAndGetRawTransactions(ctx, finalizeBlockNumber)
		
		f.finalizeBatchesWithSbb(ctx, finalizeBlockNumber)
	} else {
		// Processing transactions and finalizing batches
		f.finalizeBatches(ctx)
	}
}

func (f *finalizer) requestFinalizeBlockAndGetRawTransactions(ctx context.Context, finalizeBlockNumber uint64) {
	var platformBlockNumber *uint64

	var validSequencerAddresses []string
	var sequencerRpcUrls []string
	var leaderSequencerIndex *uint64

	var err error
	LOOPTIME := f.cfg.L2BlockMaxDeltaTimestamp.Milliseconds()

	for {
			startTime := time.Now().UnixMilli()

			Retry(ctx, func() error {
				platformBlockNumber, err = f.fetchPlatformBlockNumber(ctx)
				return err
			}, 300*time.Millisecond)

			requestPlatformBlockNumber := *platformBlockNumber - 6

			Retry(ctx, func() error {
				validSequencerAddresses, sequencerRpcUrls, leaderSequencerIndex, err = f.fetchSequencerInfo(ctx, requestPlatformBlockNumber, finalizeBlockNumber)
				return err
			}, 300*time.Millisecond)

			log.Debug("Successfully updated sequencer info")

			Retry(ctx, func() error {
				err = f.finalizeBlock(ctx, requestPlatformBlockNumber, finalizeBlockNumber, sequencerRpcUrls, leaderSequencerIndex, validSequencerAddresses)
				return err
			}, 300*time.Millisecond)


			Retry(ctx, func() error {
				txs, err := f.getRawTransactions(ctx, finalizeBlockNumber, sequencerRpcUrls, leaderSequencerIndex)
				if err != nil {
					return err
				}
				f.blockTransactions[finalizeBlockNumber] = txs

				return nil
			}, 300*time.Millisecond)

			finalizeBlockNumber++	

			endTime := time.Now().UnixMilli()
			duration := endTime - startTime
			var nextActionDelay time.Duration
			if LOOPTIME-duration > 0 {
				nextActionDelay = time.Duration(LOOPTIME - duration)
			} else {
				nextActionDelay = time.Duration(0)
			}

			time.Sleep(nextActionDelay * time.Millisecond)
	}
}

// finalizeBatches runs the endless loop for processing transactions finalizing batches.
func (f *finalizer) finalizeBatchesWithSbb(ctx context.Context, finalizeBlockNumber uint64) error {
	log.Debug("finalizer init loop with SBB")

	for {
		if f.wipL2Block.timestamp+uint64(f.cfg.L2BlockMaxDeltaTimestamp.Seconds()) <= uint64(time.Now().Unix()) {
			txs, exists := f.blockTransactions[finalizeBlockNumber]


			for !exists {
				f.workerReadyTxsCond.L.Lock()
				f.workerReadyTxsCond.WaitOrTimeout(f.cfg.NewTxsWaitInterval.Duration)
				f.workerReadyTxsCond.L.Unlock()

				txs, exists = f.blockTransactions[finalizeBlockNumber]
			}


			if len(txs) > 0 {
				fmt.Println("stompesi - submitRawTransactions - f.finalizedBlockNumber: ", finalizeBlockNumber)
				fmt.Println("stompesi - submitRawTransactions - tx_count: ", txs.Len())
				
				for _, tx := range txs {
					processBatchResponse, err := f.stateIntf.PreProcessTransaction(ctx, tx, nil)

					if err != nil {
						continue
					}

					poolTx := pool.NewTransaction(*tx, "", false)
					poolTx.ZKCounters = processBatchResponse.UsedZkCounters
					poolTx.ReservedZKCounters = processBatchResponse.ReservedZkCounters

					txTracker, _ := f.workerIntf.NewTxTracker(poolTx.Transaction, poolTx.ZKCounters, poolTx.ReservedZKCounters, poolTx.IP)

					firstTxProcess := true

					for {
						var err error
						_, err = f.processTransaction(ctx, txTracker, firstTxProcess)
						if err != nil {
							if err == ErrEffectiveGasPriceReprocess {
								firstTxProcess = false
								log.Infof("reprocessing tx %s because of effective gas price calculation", txTracker.HashStr)
								continue
							} else if err == ErrBatchResourceOverFlow {
								f.finalizeWIPBatchSbbVersion(ctx, state.ResourceMarginExhaustedClosingReason)
								continue
							} else {
								log.Errorf("failed to process tx %s, error: %v", err)
								break
							}
						}
						break
					}
				}
			}

			f.finalizeWIPL2Block(ctx)

			finalizeBlockNumber++
		}		

		idleTime := time.Now()

		// wait for new ready txs in worker
		f.workerReadyTxsCond.L.Lock()
		f.workerReadyTxsCond.WaitOrTimeout(f.cfg.NewTxsWaitInterval.Duration)
		f.workerReadyTxsCond.L.Unlock()

		// Increase idle time of the WIP L2Block
		f.wipL2Block.metrics.idleTime += time.Since(idleTime)

		if f.haltFinalizer.Load() {
			// There is a fatal error and we need to halt the finalizer and stop processing new txs
			for {
				time.Sleep(5 * time.Second) //nolint:gomnd
			}
		}

		if finalize, closeReason := f.checkIfFinalizeBatch(); finalize {
			f.finalizeWIPBatchSbbVersion(ctx, closeReason)
		}
		
		if err := ctx.Err(); err != nil {
			log.Errorf("stopping finalizer because of context, error: %v", err)
			return err
		}
	}
}

// updateProverIdAndFlushId updates the prover id and flush id
func (f *finalizer) updateProverIdAndFlushId(ctx context.Context) {
	for {
		f.pendingFlushIDCond.L.Lock()
		// f.storedFlushID is >= than f.lastPendingFlushID, this means all pending txs (flushid) are stored by the executor.
		// We are "synced" with the flush id, therefore we need to wait for new tx (new pending flush id to be stored by the executor)
		for f.storedFlushID >= f.lastPendingFlushID {
			f.pendingFlushIDCond.Wait()
		}
		f.pendingFlushIDCond.L.Unlock()

		for f.storedFlushID < f.lastPendingFlushID { //TODO: review this loop as could be is pulling all the time, no sleep
			storedFlushID, proverID, err := f.stateIntf.GetStoredFlushID(ctx)
			if err != nil {
				log.Errorf("failed to get stored flush id, error: %v", err)
			} else {
				if storedFlushID != f.storedFlushID {
					// Check if prover/Executor has been restarted
					f.checkIfProverRestarted(proverID)

					// Update f.storeFlushID and signal condition f.storedFlushIDCond
					f.storedFlushIDCond.L.Lock()
					f.storedFlushID = storedFlushID
					f.storedFlushIDCond.Broadcast()
					f.storedFlushIDCond.L.Unlock()
				}
			}
		}
	}
}

// updateFlushIDs updates f.lastPendingFLushID and f.storedFlushID with newPendingFlushID and newStoredFlushID values (it they have changed)
// and sends the signals conditions f.pendingFlushIDCond and f.storedFlushIDCond to notify other go funcs that the values have changed
func (f *finalizer) updateFlushIDs(newPendingFlushID, newStoredFlushID uint64) {
	if newPendingFlushID > f.lastPendingFlushID {
		f.lastPendingFlushID = newPendingFlushID
		f.pendingFlushIDCond.Broadcast()
	}

	f.storedFlushIDCond.L.Lock()
	if newStoredFlushID > f.storedFlushID {
		f.storedFlushID = newStoredFlushID
		f.storedFlushIDCond.Broadcast()
	}
	f.storedFlushIDCond.L.Unlock()
}

func (f *finalizer) checkL1InfoTreeUpdate(ctx context.Context) {
	firstL1InfoRootUpdate := true
	skipFirstSleep := true

	if f.cfg.L1InfoTreeCheckInterval.Duration.Seconds() == 999999 { //nolint:gomnd
		if !f.lastL1InfoTreeValid {
			f.lastL1InfoTreeCond.L.Lock()
			f.lastL1InfoTreeValid = true
			f.lastL1InfoTreeCond.Broadcast()
			f.lastL1InfoTreeCond.L.Unlock()
		}

		return
	}

	for {
		if skipFirstSleep {
			skipFirstSleep = false
		} else {
			time.Sleep(f.cfg.L1InfoTreeCheckInterval.Duration)
		}

		lastL1BlockNumber, err := f.etherman.GetLatestBlockNumber(ctx)
		if err != nil {
			log.Errorf("error getting latest L1 block number, error: %v", err)
			continue
		}

		maxBlockNumber := uint64(0)
		if f.cfg.L1InfoTreeL1BlockConfirmations <= lastL1BlockNumber {
			maxBlockNumber = lastL1BlockNumber - f.cfg.L1InfoTreeL1BlockConfirmations
		}

		l1InfoRoot, err := f.stateIntf.GetLatestL1InfoRoot(ctx, maxBlockNumber)
		if err != nil {
			log.Errorf("error checking latest L1InfoRoot, error: %v", err)
			continue
		}

		// L1InfoTreeIndex = 0 is a special case (empty tree) therefore we will set GER as zero
		if l1InfoRoot.L1InfoTreeIndex == 0 {
			l1InfoRoot.GlobalExitRoot.GlobalExitRoot = state.ZeroHash
		}

		if firstL1InfoRootUpdate || l1InfoRoot.L1InfoTreeIndex > f.lastL1InfoTree.L1InfoTreeIndex {
			log.Infof("received new L1InfoRoot, l1InfoTreeIndex: %d, l1InfoTreeRoot: %s, l1Block: %d",
				l1InfoRoot.L1InfoTreeIndex, l1InfoRoot.L1InfoTreeRoot, l1InfoRoot.BlockNumber)

			// Sanity check l1BlockState (l1InfoRoot.BlockNumber) blockhash matches blockhash on ethereum. We skip it if l1InfoRoot.BlockNumber == 0 (empty tree)
			if l1InfoRoot.BlockNumber > 0 {
				l1BlockState, err := f.stateIntf.GetBlockByNumber(ctx, l1InfoRoot.BlockNumber, nil)
				if err != nil {
					log.Errorf("error getting L1 block %d from the state, error: %v", l1InfoRoot.BlockNumber, err)
					continue
				}

				l1BlockEth, err := f.etherman.HeaderByNumber(ctx, new(big.Int).SetUint64(l1InfoRoot.BlockNumber))
				if err != nil {
					log.Errorf("error getting L1 block %d from ethereum, error: %v", l1InfoRoot.BlockNumber, err)
					continue
				}
				if l1BlockState.BlockHash != l1BlockEth.Hash() {
					warnmsg := fmt.Sprintf("invalid l1InfoTreeIndex %d, L1 block %d blockhash %s doesn't match blockhash on ethereum %s (L1 reorg?). Stopping syncing l1IntroTreeIndex",
						l1InfoRoot.L1InfoTreeIndex, l1InfoRoot.BlockNumber, l1BlockState.BlockHash, l1BlockEth.Hash())
					log.Warn(warnmsg)
					f.LogEvent(ctx, event.Level_Critical, event.EventID_InvalidInfoRoot, warnmsg, nil)
					return
				}
			}

			firstL1InfoRootUpdate = false

			f.lastL1InfoTreeMux.Lock()
			f.lastL1InfoTree = l1InfoRoot
			f.lastL1InfoTreeMux.Unlock()

			if !f.lastL1InfoTreeValid {
				f.lastL1InfoTreeCond.L.Lock()
				f.lastL1InfoTreeValid = true
				f.lastL1InfoTreeCond.Broadcast()
				f.lastL1InfoTreeCond.L.Unlock()
			}
		}
	}
}

func hexToTransaction(str string) (*types.Transaction, error) {
	transaction := new(types.Transaction)

	binary, err := hex.DecodeHex(str)
	if err != nil {
		return nil, err
	}

	if err := transaction.UnmarshalBinary(binary); err != nil {
		return nil, err
	}

	return transaction, nil
}

func (f *finalizer) fetchPlatformBlockNumber(ctx context.Context) (*uint64, error) {
	reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second) // TODO: configuration
	defer reqCancel()

	platformBlockNumber, err := f.ethClient.BlockNumber(reqCtx)
	if err != nil {
		log.Error("failed to fetch platform block number", "error", err.Error())
		return nil, err
	}

	return &platformBlockNumber, nil
}

func (f *finalizer) fetchSequencerInfo(ctx context.Context, platformBlockNumber uint64, finalizeBlockNumber uint64) ([]string, []string, *uint64, error) {
	sequencerAddresses, err := f.fetchSequencerAddresses(ctx, platformBlockNumber)
	if err != nil {
		log.Error("failed to fetch sequencer addresses ", "error ", err.Error())
		return nil, nil, nil, err
	}

	validSequencerAddresses, sequencerRpcUrls, err := f.fetchSequencerRpcUrls(ctx, sequencerAddresses)
	if err != nil {
		log.Error("failed to fetch sequencer rpc urls ", "error ", err.Error())
		return nil, nil, nil, err
	}

	log.Debug("Successfully fetched sequencer info", " sequencerAddresses: ", sequencerAddresses, " sequencerRpcUrls: ", sequencerRpcUrls)

	leaderSequencerIndex, err := f.getLeaderSequencerIndex(finalizeBlockNumber, sequencerRpcUrls)
	if err != nil {
		log.Error("failed to get leader sequencer index ", "error ", err.Error())
		return nil, nil, nil, err
	}

	log.Debug("Successfully fetched leader sequencer index", " leaderSequencerIndex: ", *leaderSequencerIndex)

	return validSequencerAddresses, sequencerRpcUrls, leaderSequencerIndex, nil
}

func (f *finalizer) getLeaderSequencerIndex(finalizeBlockNumber uint64, sequencerRpcUrls []string) (*uint64, error) {

	if len(sequencerRpcUrls) < 1 {
		return nil, errors.New("there are no URLs available, making modular arithmetic impossible")
	}

	for i := 0; i < len(sequencerRpcUrls); i++ {
		if sequencerRpcUrls[i] == "http://210.222.63.26:5000" {
			index := uint64(i) 
			return &index, nil
		}
	}

	mod := finalizeBlockNumber % uint64(len(sequencerRpcUrls))
	return &mod, nil
}

func (f *finalizer) fetchSequencerAddresses(ctx context.Context, platformBlockNumber uint64) ([]string, error) {
	reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second)
	defer reqCancel()

	contractAbi, err := abi.JSON(strings.NewReader(abiString))
	if err != nil {
		return nil, err
	}

	METHOD := "getSequencers"
	contractAddress := common.HexToAddress(f.cfg.LivenessContractAddress)

	data, err := contractAbi.Pack(METHOD, f.cfg.ClusterId)
	if err != nil {
		return nil, err
	}

	query := ethereum.CallMsg{
		To:   &contractAddress,
		Data: data,
	}

	result, err := f.ethClient.CallContract(reqCtx, query, big.NewInt(int64(platformBlockNumber)))
	if err != nil {
		log.Error("failed to make a contract call to retrieve the sequencer URL list", "error", err.Error())

		return nil, err
	}

	var sequencerList []common.Address
	if contractAbi.UnpackIntoInterface(&sequencerList, METHOD, result) != nil {
		return nil, err
	}

	var sequencerAddresses []string
	for _, addr := range sequencerList {
		if addr != common.HexToAddress("0x0000000000000000000000000000000000000000") {
			sequencerAddresses = append(sequencerAddresses, addr.Hex())
		}
	}

	return sequencerAddresses, nil
}

func (f *finalizer) fetchSequencerRpcUrls(ctx context.Context, sequencerAddresses []string) ([]string, []string, error) {
	reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second)
	defer reqCancel()

	body := newJsonRpcRequest(GetSequencerRpcUrlList, GetSequencerRpcUrlsParams{
		SequencerAddresses: sequencerAddresses,
	})

	res := &GetSequencerRpcUrlsResponse{}
	if err := f.sbbClient.Send(reqCtx, f.cfg.SeedNodeUrl, body, res); err != nil {
		log.Error("failed to send get_sequencer_rpc_url_list request to seeder node", "error", err.Error())

		return nil, nil, err
	}

	var validSequencerAddresses []string
	var sequencerRpcUrls []string
	for _, sequencerRpcUrl := range res.SequencerRpcUrls {
		if sequencerRpcUrl.ClusterRpcUrl != "" {
			validSequencerAddresses = append(validSequencerAddresses, sequencerRpcUrl.Address)
			sequencerRpcUrls = append(sequencerRpcUrls, sequencerRpcUrl.ClusterRpcUrl)
		}
	}

	return validSequencerAddresses, sequencerRpcUrls, nil
}



func (f *finalizer) getNextLeaderSequencerIndex(sequencerCount uint64, currentLeaderSeqeuncerIndex uint64) (*uint64, error) {
	if sequencerCount < 1 {
		return nil, errors.New("cannot divide by zero")
	}

	nextLeaderSequencerIndex := (currentLeaderSeqeuncerIndex + 1) % sequencerCount

	return &nextLeaderSequencerIndex, nil
}

func (f *finalizer) increaseLeaderSequencerIndex(sequencerCount uint64, leaderSequencerIndex *uint64) error {
	if sequencerCount < 1 {
		return errors.New("cannot divide by zero")
	}

	*leaderSequencerIndex = (*leaderSequencerIndex + 1) % sequencerCount

	return nil
}

func (f *finalizer) finalizeBlock(ctx context.Context, platformBlockNumber uint64, finalizeBlockNumber uint64, sequencerRpcUrls []string, leaderSequencerIndex *uint64, sequencerAddresses []string) error {

	sequencerCount := len(sequencerRpcUrls)

	for i := 0; i < sequencerCount; i++ {
		nextSequencerIndex, err := f.getNextLeaderSequencerIndex(uint64(sequencerCount), *leaderSequencerIndex)
		if err != nil {
			return err
		}

		message := FinalizeBlockMessageParams{
			RollupId:        f.cfg.RollupId,
			ExecutorAddress: f.sequencerAddress,

			PlatformBlockHeight: platformBlockNumber,
			RollupBlockHeight:   finalizeBlockNumber,
			
      BlockCreatorAddress:     strings.ToLower(sequencerAddresses[*leaderSequencerIndex]),
			NextBlockCreatorAddress: strings.ToLower(sequencerAddresses[*nextSequencerIndex]),
		}

		messageBytes, err := json.Marshal(message)
		if err != nil {
			log.Error("Error converting message to bytes: %v", err)
			return err
		}

		h := keccak256.Hash(messageBytes)

		signature, err := crypto.Sign(h, f.sequencerPrivateKey)
		if err != nil {
			log.Error("Error signing message: %v", err)
			return err
		}

		params := FinalizeBlockParams{
			Message:   message,
			Signature: "0x" + common.Bytes2Hex(signature),
		}

		log.Debug("Finalizing the contents to be included in the block", "block number: ", finalizeBlockNumber, "i", i)

		body := newJsonRpcRequest(FinalizeBlock, params)

		reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second)
		defer reqCancel()

		if err = f.sbbClient.Send(reqCtx, sequencerRpcUrls[*leaderSequencerIndex], body, nil); err != nil {
			if !strings.Contains(err.Error(), "connection refused") {
				return fmt.Errorf("failed to send finalize_block request to SBB: %s request params: platformHeight %d rollupHeight %d url %s now %d", err.Error(), message.PlatformBlockHeight, message.RollupBlockHeight, sequencerRpcUrls[*leaderSequencerIndex], time.Now().UnixMilli())
			}

			log.Warn("failed to finalizing due to no sequencer found. retrying with a different sequencer")

			if err = f.increaseLeaderSequencerIndex(uint64(sequencerCount), leaderSequencerIndex); err != nil {
				return err
			}

			log.Debug("stopesi - Error", err)

			continue
		}

		log.Debug("Successfully finalized the contents to be included in the block. ", "block number: ", finalizeBlockNumber, " now: ", time.Now().UnixMilli())

		return nil
	}

	return errors.New("no sequencer")
}

func (f *finalizer) getRawTransactions(ctx context.Context, finalizedBlockNumber uint64, sequencerRpcUrls []string, leaderSequencerIndex *uint64) (types.Transactions, error) {
	reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Second)
	defer reqCancel()

	params := GetRawTransactionsParams{
		RollupId:          f.cfg.RollupId,
		RollupBlockHeight: finalizedBlockNumber,
	}

	body := newJsonRpcRequest(GetRawTransactionList, params)

	res := &GetRawTransactionsResponse{}
	sequencerCount := len(sequencerRpcUrls)

	for i := 0; i < sequencerCount; i++ {
		if err := f.sbbClient.Send(reqCtx, sequencerRpcUrls[*leaderSequencerIndex], body, res); err != nil {
			if !strings.Contains(err.Error(), "connection refused") {
				return nil, fmt.Errorf("failed to send get_raw_transaction_list request to SBB: %s height %d url %s now %d", err.Error(), params.RollupBlockHeight, sequencerRpcUrls[*leaderSequencerIndex], time.Now().UnixMilli())
			}

			log.Warn("failed to get raw transactions due to no sequencer found. retrying with a different sequencer")

			if err = f.increaseLeaderSequencerIndex(uint64(sequencerCount), leaderSequencerIndex); err != nil {
				return nil, err
			}
		}

		var transactions []*types.Transaction
		for _, hexString := range res.RawTransactions {
			transaction, _ := hexToTransaction(hexString)
			transactions = append(transactions, transaction)
		}
		return transactions, nil
	}

	return nil, errors.New("no sequencer")
}

func Retry(ctx context.Context, fn func() error, retryInterval time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := fn()
		if err == nil {
			return
		}
		time.Sleep(retryInterval)
	}
}

// finalizeBatches runs the endless loop for processing transactions finalizing batches.
func (f *finalizer) finalizeBatches(ctx context.Context) {
	log.Debug("finalizer init loop")
	showNotFoundTxLog := true // used to log debug only the first message when there is no txs to process
	for {
		// We have reached the L2 block time, we need to close the current L2 block and open a new one
		if f.wipL2Block.timestamp+uint64(f.cfg.L2BlockMaxDeltaTimestamp.Seconds()) <= uint64(time.Now().Unix()) {
			f.finalizeWIPL2Block(ctx)
		}

		tx, err := f.workerIntf.GetBestFittingTx(f.wipBatch.imRemainingResources)

		// If we have txs pending to process but none of them fits into the wip batch, we close the wip batch and open a new one
		if err == ErrNoFittingTransaction {
			f.finalizeWIPBatch(ctx, state.NoTxFitsClosingReason)
			continue
		}

		if tx != nil {
			showNotFoundTxLog = true

			firstTxProcess := true

			for {
				var err error
				_, err = f.processTransaction(ctx, tx, firstTxProcess)
				if err != nil {
					if err == ErrEffectiveGasPriceReprocess {
						firstTxProcess = false
						log.Infof("reprocessing tx %s because of effective gas price calculation", tx.HashStr)
						continue
					} else if err == ErrBatchResourceOverFlow {
						log.Infof("skipping tx %s due to a batch resource overflow", tx.HashStr)
						break
					} else {
						log.Errorf("failed to process tx %s, error: %v", err)
						break
					}
				}
				break
			}
		} else {
			idleTime := time.Now()

			if showNotFoundTxLog {
				log.Debug("no transactions to be processed. Waiting...")
				showNotFoundTxLog = false
			}

			// wait for new ready txs in worker
			f.workerReadyTxsCond.L.Lock()
			f.workerReadyTxsCond.WaitOrTimeout(f.cfg.NewTxsWaitInterval.Duration)
			f.workerReadyTxsCond.L.Unlock()

			// Increase idle time of the WIP L2Block
			f.wipL2Block.metrics.idleTime += time.Since(idleTime)
		}

		if f.haltFinalizer.Load() {
			// There is a fatal error and we need to halt the finalizer and stop processing new txs
			for {
				time.Sleep(5 * time.Second) //nolint:gomnd
			}
		}

		// Check if we must finalize the batch due to a closing reason (resources exhausted, max txs, timestamp resolution, forced batches deadline)
		if finalize, closeReason := f.checkIfFinalizeBatch(); finalize {
			f.finalizeWIPBatch(ctx, closeReason)
		}

		if err := ctx.Err(); err != nil {
			log.Errorf("stopping finalizer because of context, error: %v", err)
			return
		}
	}
}

// processTransaction processes a single transaction.
func (f *finalizer) processTransaction(ctx context.Context, tx *TxTracker, firstTxProcess bool) (errWg *sync.WaitGroup, err error) {
	start := time.Now()

	log.Infof("processing tx %s, batchNumber: %d, l2Block: [%d], oldStateRoot: %s, L1InfoRootIndex: %d",
		tx.HashStr, f.wipBatch.batchNumber, f.wipL2Block.trackingNum, f.wipBatch.imStateRoot, f.wipL2Block.l1InfoTreeExitRoot.L1InfoTreeIndex)

	batchRequest := state.ProcessRequest{
		BatchNumber:               f.wipBatch.batchNumber,
		OldStateRoot:              f.wipBatch.imStateRoot,
		Coinbase:                  f.wipBatch.coinbase,
		L1InfoRoot_V2:             state.GetMockL1InfoRoot(),
		TimestampLimit_V2:         f.wipL2Block.timestamp,
		Caller:                    stateMetrics.DiscardCallerLabel,
		ForkID:                    f.stateIntf.GetForkIDByBatchNumber(f.wipBatch.batchNumber),
		Transactions:              tx.RawTx,
		SkipFirstChangeL2Block_V2: true,
		SkipWriteBlockInfoRoot_V2: true,
		SkipVerifyL1InfoRoot_V2:   true,
		L1InfoTreeData_V2:         map[uint32]state.L1DataV2{},
		ExecutionMode:             executor.ExecutionMode0,
	}

	txGasPrice := tx.GasPrice

	// If it is the first time we process this tx then we calculate the EffectiveGasPrice
	if firstTxProcess {
		// Get L1 gas price and store in txTracker to make it consistent during the lifespan of the transaction
		tx.L1GasPrice, tx.L2GasPrice = f.poolIntf.GetL1AndL2GasPrice()
		// Get the tx and l2 gas price we will use in the egp calculation. If egp is disabled we will use a "simulated" tx gas price
		txGasPrice, txL2GasPrice := f.effectiveGasPrice.GetTxAndL2GasPrice(tx.GasPrice, tx.L1GasPrice, tx.L2GasPrice)

		// Save values for later logging
		tx.EGPLog.L1GasPrice = tx.L1GasPrice
		tx.EGPLog.L2GasPrice = txL2GasPrice
		tx.EGPLog.GasUsedFirst = tx.UsedZKCounters.GasUsed
		tx.EGPLog.GasPrice.Set(txGasPrice)

		// Calculate EffectiveGasPrice
		egp, err := f.effectiveGasPrice.CalculateEffectiveGasPrice(tx.RawTx, txGasPrice, tx.UsedZKCounters.GasUsed, tx.L1GasPrice, txL2GasPrice)
		if err != nil {
			if f.effectiveGasPrice.IsEnabled() {
				return nil, err
			} else {
				log.Warnf("effectiveGasPrice is disabled, but failed to calculate effectiveGasPrice for tx %s, error: %v", tx.HashStr, err)
				tx.EGPLog.Error = fmt.Sprintf("CalculateEffectiveGasPrice#1: %s", err)
			}
		} else {
			tx.EffectiveGasPrice.Set(egp)

			// Save first EffectiveGasPrice for later logging
			tx.EGPLog.ValueFirst.Set(tx.EffectiveGasPrice)

			// If EffectiveGasPrice >= txGasPrice, we process the tx with tx.GasPrice
			if tx.EffectiveGasPrice.Cmp(txGasPrice) >= 0 {
				loss := new(big.Int).Sub(tx.EffectiveGasPrice, txGasPrice)
				// If loss > 0 the warning message indicating we loss fee for thix tx
				if loss.Cmp(new(big.Int).SetUint64(0)) == 1 {
					log.Warnf("egp-loss: gasPrice: %d, effectiveGasPrice1: %d, loss: %d, tx: %s", txGasPrice, tx.EffectiveGasPrice, loss, tx.HashStr)
				}

				tx.EffectiveGasPrice.Set(txGasPrice)
				tx.IsLastExecution = true
			}
		}
	}

	egpPercentage, err := f.effectiveGasPrice.CalculateEffectiveGasPricePercentage(txGasPrice, tx.EffectiveGasPrice)
	if err != nil {
		if f.effectiveGasPrice.IsEnabled() {
			return nil, err
		} else {
			log.Warnf("effectiveGasPrice is disabled, but failed to to calculate efftive gas price percentage (#1), error: %v", err)
			tx.EGPLog.Error = fmt.Sprintf("%s; CalculateEffectiveGasPricePercentage#1: %s", tx.EGPLog.Error, err)
		}
	} else {
		// Save percentage for later logging
		tx.EGPLog.Percentage = egpPercentage
	}

	// If EGP is disabled we use tx GasPrice (MaxEffectivePercentage=255)
	if !f.effectiveGasPrice.IsEnabled() {
		egpPercentage = state.MaxEffectivePercentage
	}

	// Assign applied EGP percentage to tx (TxTracker)
	tx.EGPPercentage = egpPercentage

	effectivePercentageAsDecodedHex, err := hex.DecodeHex(fmt.Sprintf("%x", tx.EGPPercentage))
	if err != nil {
		return nil, err
	}

	batchRequest.Transactions = append(batchRequest.Transactions, effectivePercentageAsDecodedHex...)

	executionStart := time.Now()
	batchResponse, err := f.stateIntf.ProcessBatchV2(ctx, batchRequest, false)
	executionTime := time.Since(executionStart)
	f.wipL2Block.metrics.transactionsTimes.executor += executionTime

	if err != nil && (errors.Is(err, runtime.ErrExecutorDBError) || errors.Is(err, runtime.ErrInvalidTxChangeL2BlockMinTimestamp)) {
		log.Errorf("failed to process tx %s, error: %v", tx.HashStr, err)
		return nil, err
	} else if err == nil && !batchResponse.IsRomLevelError && len(batchResponse.BlockResponses) == 0 {
		err = fmt.Errorf("executor returned no errors and no responses for tx %s", tx.HashStr)
		f.Halt(ctx, err, false)
	} else if err != nil {
		log.Errorf("error received from executor, error: %v", err)

		// Delete tx from the worker
		f.workerIntf.DeleteTx(tx.Hash, tx.From)

		// Set tx as invalid in the pool
		errMsg := err.Error()
		err = f.poolIntf.UpdateTxStatus(ctx, tx.Hash, pool.TxStatusInvalid, false, &errMsg)
		if err != nil {
			log.Errorf("failed to update status to invalid in the pool for tx %s, error: %v", tx.Hash.String(), err)
		}
		return nil, err
	}

	oldStateRoot := f.wipBatch.imStateRoot
	if len(batchResponse.BlockResponses) > 0 {
		errWg, err = f.handleProcessTransactionResponse(ctx, tx, batchResponse, oldStateRoot)
		if err != nil {
			return errWg, err
		}
	}

	// Update imStateRoot
	f.wipBatch.imStateRoot = batchResponse.NewStateRoot

	log.Infof("processed tx %s, batchNumber: %d, l2Block: [%d], newStateRoot: %s, oldStateRoot: %s, time: {process: %v, executor: %v}, used counters: %s, reserved counters: %s",
		tx.HashStr, batchRequest.BatchNumber, f.wipL2Block.trackingNum, batchResponse.NewStateRoot.String(), batchRequest.OldStateRoot.String(),
		time.Since(start), executionTime, f.logZKCounters(batchResponse.UsedZkCounters), f.logZKCounters(batchResponse.ReservedZkCounters))

	return nil, nil
}

// handleProcessTransactionResponse handles the response of transaction processing.
func (f *finalizer) handleProcessTransactionResponse(ctx context.Context, tx *TxTracker, result *state.ProcessBatchResponse, oldStateRoot common.Hash) (errWg *sync.WaitGroup, err error) {
	txResponse := result.BlockResponses[0].TransactionResponses[0]

	// Update metrics
	f.wipL2Block.metrics.processedTxsCount++

	// Handle Transaction Error
	errorCode := executor.RomErrorCode(txResponse.RomError)
	if !state.IsStateRootChanged(errorCode) {
		// If intrinsic error or OOC error, we skip adding the transaction to the batch
		errWg = f.handleProcessTransactionError(ctx, result, tx)
		return errWg, txResponse.RomError
	}

	egpEnabled := f.effectiveGasPrice.IsEnabled()

	if !tx.IsLastExecution {
		tx.IsLastExecution = true

		// Get the tx gas price we will use in the egp calculation. If egp is disabled we will use a "simulated" tx gas price
		txGasPrice, txL2GasPrice := f.effectiveGasPrice.GetTxAndL2GasPrice(tx.GasPrice, tx.L1GasPrice, tx.L2GasPrice)

		newEffectiveGasPrice, err := f.effectiveGasPrice.CalculateEffectiveGasPrice(tx.RawTx, txGasPrice, txResponse.GasUsed, tx.L1GasPrice, txL2GasPrice)
		if err != nil {
			if egpEnabled {
				log.Errorf("failed to calculate effective gas price with new gasUsed for tx %s, error: %v", tx.HashStr, err.Error())
				return nil, err
			} else {
				log.Warnf("effectiveGasPrice is disabled, but failed to calculate effective gas price with new gasUsed for tx %s, error: %v", tx.HashStr, err.Error())
				tx.EGPLog.Error = fmt.Sprintf("%s; CalculateEffectiveGasPrice#2: %s", tx.EGPLog.Error, err)
			}
		} else {
			// Save new (second) gas used and second effective gas price calculation for later logging
			tx.EGPLog.ValueSecond.Set(newEffectiveGasPrice)
			tx.EGPLog.GasUsedSecond = txResponse.GasUsed

			errCompare := f.compareTxEffectiveGasPrice(ctx, tx, newEffectiveGasPrice, txResponse.HasGaspriceOpcode, txResponse.HasBalanceOpcode)

			// If EffectiveGasPrice is disabled we will calculate the percentage and save it for later logging
			if !egpEnabled {
				effectivePercentage, err := f.effectiveGasPrice.CalculateEffectiveGasPricePercentage(txGasPrice, tx.EffectiveGasPrice)
				if err != nil {
					log.Warnf("effectiveGasPrice is disabled, but failed to calculate effective gas price percentage (#2), error: %v", err)
					tx.EGPLog.Error = fmt.Sprintf("%s, CalculateEffectiveGasPricePercentage#2: %s", tx.EGPLog.Error, err)
				} else {
					// Save percentage for later logging
					tx.EGPLog.Percentage = effectivePercentage
				}
			}

			if errCompare != nil && egpEnabled {
				return nil, errCompare
			}
		}
	}

	// Check if reserved resources of the tx fits in the remaining batch resources
	subOverflow := false
	fits, overflowResource := f.wipBatch.imRemainingResources.Fits(state.BatchResources{ZKCounters: result.ReservedZkCounters, Bytes: uint64(len(tx.RawTx))})
	if fits {
		// Subtract the used resources from the batch
		subOverflow, overflowResource = f.wipBatch.imRemainingResources.Sub(state.BatchResources{ZKCounters: result.UsedZkCounters, Bytes: uint64(len(tx.RawTx))})
		if subOverflow { // Sanity check, this cannot happen as reservedZKCounters should be >= that usedZKCounters
			sLog := fmt.Sprintf("tx %s used resources exceeds the remaining batch resources, overflow resource: %s, updating metadata for tx in worker and continuing. Batch counters: %s, tx used counters: %s",
				tx.HashStr, overflowResource, f.logZKCounters(f.wipBatch.imRemainingResources.ZKCounters), f.logZKCounters(result.UsedZkCounters))

			log.Errorf(sLog)

			f.LogEvent(ctx, event.Level_Error, event.EventID_UsedZKCountersOverflow, sLog, nil)
		}
	} else {
		log.Infof("current tx %s reserved resources exceeds the remaining batch resources, overflow resource: %s, updating metadata for tx in worker and continuing. Batch counters: %s, tx reserved counters: %s",
			tx.HashStr, overflowResource, f.logZKCounters(f.wipBatch.imRemainingResources.ZKCounters), f.logZKCounters(result.ReservedZkCounters))
		if !f.batchConstraints.IsWithinConstraints(result.ReservedZkCounters) {
			log.Infof("current tx %s reserved resources exceeds the max limit for batch resources (node OOC), setting tx as invalid in the pool", tx.HashStr)

			f.LogEvent(ctx, event.Level_Info, event.EventID_NodeOOC,
				fmt.Sprintf("tx %s exceeds node max limit batch resources (node OOC), from: %s, IP: %s", tx.HashStr, tx.FromStr, tx.IP), nil)

			// Delete the transaction from the txSorted list
			f.workerIntf.DeleteTx(tx.Hash, tx.From)

			errMsg := "node OOC"
			err = f.poolIntf.UpdateTxStatus(ctx, tx.Hash, pool.TxStatusInvalid, false, &errMsg)
			if err != nil {
				log.Errorf("failed to update status to invalid in the pool for tx %s, error: %v", tx.Hash.String(), err)
			}

			return nil, ErrBatchResourceOverFlow
		}
	}

	// If reserved tx resources don't fit in the remaining batch resources (or we got an overflow when trying to subtract the used resources)
	// we update the ZKCounters of the tx and returns ErrBatchResourceOverFlow error
	if !fits || subOverflow {
		f.workerIntf.UpdateTxZKCounters(txResponse.TxHash, tx.From, result.UsedZkCounters, result.ReservedZkCounters)
		return nil, ErrBatchResourceOverFlow
	}

	// Save Enabled, GasPriceOC, BalanceOC and final effective gas price for later logging
	tx.EGPLog.Enabled = egpEnabled
	tx.EGPLog.GasPriceOC = txResponse.HasGaspriceOpcode
	tx.EGPLog.BalanceOC = txResponse.HasBalanceOpcode
	tx.EGPLog.ValueFinal.Set(tx.EffectiveGasPrice)

	// Log here the results of EGP calculation
	log.Infof("egp-log: final: %d, first: %d, second: %d, percentage: %d, deviation: %d, maxDeviation: %d, gasUsed1: %d, gasUsed2: %d, gasPrice: %d, l1GasPrice: %d, l2GasPrice: %d, reprocess: %t, gasPriceOC: %t, balanceOC: %t, enabled: %t, txSize: %d, tx: %s, error: %s",
		tx.EGPLog.ValueFinal, tx.EGPLog.ValueFirst, tx.EGPLog.ValueSecond, tx.EGPLog.Percentage, tx.EGPLog.FinalDeviation, tx.EGPLog.MaxDeviation, tx.EGPLog.GasUsedFirst, tx.EGPLog.GasUsedSecond,
		tx.EGPLog.GasPrice, tx.EGPLog.L1GasPrice, tx.EGPLog.L2GasPrice, tx.EGPLog.Reprocess, tx.EGPLog.GasPriceOC, tx.EGPLog.BalanceOC, egpEnabled, len(tx.RawTx), tx.HashStr, tx.EGPLog.Error)

	f.wipL2Block.addTx(tx)

	f.wipBatch.countOfTxs++

	f.updateWorkerAfterSuccessfulProcessing(ctx, tx.Hash, tx.From, false, result)

	// Update metrics
	f.wipL2Block.metrics.gas += txResponse.GasUsed

	return nil, nil
}

// compareTxEffectiveGasPrice compares newEffectiveGasPrice with tx.EffectiveGasPrice.
// It returns ErrEffectiveGasPriceReprocess if the tx needs to be reprocessed with
// the tx.EffectiveGasPrice updated, otherwise it returns nil
func (f *finalizer) compareTxEffectiveGasPrice(ctx context.Context, tx *TxTracker, newEffectiveGasPrice *big.Int, hasGasPriceOC bool, hasBalanceOC bool) error {
	// Get the tx gas price we will use in the egp calculation. If egp is disabled we will use a "simulated" tx gas price
	txGasPrice, _ := f.effectiveGasPrice.GetTxAndL2GasPrice(tx.GasPrice, tx.L1GasPrice, tx.L2GasPrice)

	// Compute the absolute difference between tx.EffectiveGasPrice - newEffectiveGasPrice
	diff := new(big.Int).Abs(new(big.Int).Sub(tx.EffectiveGasPrice, newEffectiveGasPrice))
	// Compute max deviation allowed of newEffectiveGasPrice
	maxDeviation := new(big.Int).Div(new(big.Int).Mul(tx.EffectiveGasPrice, new(big.Int).SetUint64(f.effectiveGasPrice.GetFinalDeviation())), big.NewInt(100)) //nolint:gomnd

	// Save FinalDeviation (diff) and MaxDeviation for later logging
	tx.EGPLog.FinalDeviation.Set(diff)
	tx.EGPLog.MaxDeviation.Set(maxDeviation)

	// if (diff > finalDeviation)
	if diff.Cmp(maxDeviation) == 1 {
		// if newEfectiveGasPrice < txGasPrice
		if newEffectiveGasPrice.Cmp(txGasPrice) == -1 {
			if hasGasPriceOC || hasBalanceOC {
				tx.EffectiveGasPrice.Set(txGasPrice)
			} else {
				tx.EffectiveGasPrice.Set(newEffectiveGasPrice)
			}
		} else {
			tx.EffectiveGasPrice.Set(txGasPrice)

			loss := new(big.Int).Sub(newEffectiveGasPrice, txGasPrice)
			// If loss > 0 the warning message indicating we loss fee for thix tx
			if loss.Cmp(new(big.Int).SetUint64(0)) == 1 {
				log.Warnf("egp-loss: gasPrice: %d, EffectiveGasPrice2: %d, loss: %d, tx: %s", txGasPrice, newEffectiveGasPrice, loss, tx.HashStr)
			}
		}

		// Save Reprocess for later logging
		tx.EGPLog.Reprocess = true

		return ErrEffectiveGasPriceReprocess
	} // else (diff <= finalDeviation) it is ok, no reprocess of the tx is needed

	return nil
}

func (f *finalizer) updateWorkerAfterSuccessfulProcessing(ctx context.Context, txHash common.Hash, txFrom common.Address, isForced bool, result *state.ProcessBatchResponse) {
	// Delete the transaction from the worker
	if isForced {
		f.workerIntf.DeleteForcedTx(txHash, txFrom)
		log.Debugf("forced tx %s deleted from address %s", txHash.String(), txFrom.Hex())
		return
	} else {
		f.workerIntf.DeleteTx(txHash, txFrom)
		log.Debugf("tx %s deleted from address %s", txHash.String(), txFrom.Hex())
	}

	txsToDelete := f.workerIntf.UpdateAfterSingleSuccessfulTxExecution(txFrom, result.ReadWriteAddresses)
	for _, txToDelete := range txsToDelete {
		err := f.poolIntf.UpdateTxStatus(ctx, txToDelete.Hash, pool.TxStatusFailed, false, txToDelete.FailedReason)
		if err != nil {
			log.Errorf("failed to update status to failed in the pool for tx %s, error: %v", txToDelete.Hash.String(), err)
			continue
		}
	}
}

// handleProcessTransactionError handles the error of a transaction
func (f *finalizer) handleProcessTransactionError(ctx context.Context, result *state.ProcessBatchResponse, tx *TxTracker) *sync.WaitGroup {
	txResponse := result.BlockResponses[0].TransactionResponses[0]
	errorCode := executor.RomErrorCode(txResponse.RomError)
	addressInfo := result.ReadWriteAddresses[tx.From]
	log.Infof("rom error in tx %s, errorCode: %d", tx.HashStr, errorCode)
	wg := new(sync.WaitGroup)
	failedReason := executor.RomErr(errorCode).Error()
	if executor.IsROMOutOfCountersError(errorCode) {
		log.Errorf("ROM out of counters error, marking tx %s as invalid, errorCode: %d", tx.HashStr, errorCode)

		f.workerIntf.DeleteTx(tx.Hash, tx.From)

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.poolIntf.UpdateTxStatus(ctx, tx.Hash, pool.TxStatusInvalid, false, &failedReason)
			if err != nil {
				log.Errorf("failed to update status to invalid in the pool for tx %s, error: %v", tx.HashStr, err)
			}
		}()
	} else if executor.IsInvalidNonceError(errorCode) || executor.IsInvalidBalanceError(errorCode) {
		var (
			nonce   *uint64
			balance *big.Int
		)
		if addressInfo != nil {
			nonce = addressInfo.Nonce
			balance = addressInfo.Balance
		}
		log.Errorf("intrinsic error, moving tx %s to not ready: nonce: %d, balance: %d. gasPrice: %d, error: %v", tx.Hash, nonce, balance, tx.GasPrice, txResponse.RomError)
		txsToDelete := f.workerIntf.MoveTxToNotReady(tx.Hash, tx.From, nonce, balance)
		for _, txToDelete := range txsToDelete {
			wg.Add(1)
			txToDelete := txToDelete
			go func() {
				defer wg.Done()
				err := f.poolIntf.UpdateTxStatus(ctx, txToDelete.Hash, pool.TxStatusFailed, false, &failedReason)
				if err != nil {
					log.Errorf("failed to update status to failed in the pool for tx %s, error: %v", txToDelete.Hash.String(), err)
				}
			}()
		}
	} else {
		// Delete the transaction from the txSorted list
		f.workerIntf.DeleteTx(tx.Hash, tx.From)
		log.Debugf("tx %s deleted from txSorted list", tx.HashStr)

		wg.Add(1)
		go func() {
			defer wg.Done()
			// Update the status of the transaction to failed
			err := f.poolIntf.UpdateTxStatus(ctx, tx.Hash, pool.TxStatusFailed, false, &failedReason)
			if err != nil {
				log.Errorf("failed to update status to failed in the pool for tx %s, error: %v", tx.Hash.String(), err)
			}
		}()
	}

	// Update metrics
	f.wipL2Block.metrics.gas += txResponse.GasUsed

	return wg
}

// checkIfProverRestarted checks if the proverID changed
func (f *finalizer) checkIfProverRestarted(proverID string) {
	if f.proverID != "" && f.proverID != proverID {
		f.LogEvent(context.Background(), event.Level_Critical, event.EventID_FinalizerRestart,
			fmt.Sprintf("proverID changed from %s to %s, restarting sequencer to discard current WIP batch and work with new executor", f.proverID, proverID), nil)

		log.Fatal("proverID changed from %s to %s, restarting sequencer to discard current WIP batch and work with new executor")
	}
}

// logZKCounters returns a string with all the zkCounters values
func (f *finalizer) logZKCounters(counters state.ZKCounters) string {
	return fmt.Sprintf("{gasUsed: %d, keccakHashes: %d, poseidonHashes: %d, poseidonPaddings: %d, memAligns: %d, arithmetics: %d, binaries: %d, sha256Hashes: %d, steps: %d}",
		counters.GasUsed, counters.KeccakHashes, counters.PoseidonHashes, counters.PoseidonPaddings, counters.MemAligns, counters.Arithmetics,
		counters.Binaries, counters.Sha256Hashes_V2, counters.Steps)
}

// Halt halts the finalizer
func (f *finalizer) Halt(ctx context.Context, err error, isFatal bool) {
	f.haltFinalizer.Store(true)

	f.LogEvent(ctx, event.Level_Critical, event.EventID_FinalizerHalt, fmt.Sprintf("finalizer halted due to error, error: %s", err), nil)

	if isFatal {
		log.Fatalf("fatal error on finalizer, error: %v", err)
	} else {
		for {
			log.Errorf("halting finalizer, error: %v", err)
			time.Sleep(5 * time.Second) //nolint:gomnd
		}
	}
}

// LogEvent adds an event for runtime debugging
func (f *finalizer) LogEvent(ctx context.Context, level event.Level, eventId event.EventID, description string, json interface{}) {
	event := &event.Event{
		ReceivedAt:  time.Now(),
		Source:      event.Source_Node,
		Component:   event.Component_Sequencer,
		Level:       level,
		EventID:     eventId,
		Description: description,
	}

	if json != nil {
		event.Json = json
	}

	eventErr := f.eventLog.LogEvent(ctx, event)
	if eventErr != nil {
		log.Errorf("error storing log event, error: %v", eventErr)
	}
}

type JSONRPCRequest[T any] struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  T      `json:"params"`
	ID      int    `json:"id"`
}

func newJsonRpcRequest[T any](method Method, params T) JSONRPCRequest[T] {
	return JSONRPCRequest[T]{
		JSONRPC: "2.0",
		Method:  string(method),
		Params:  params,
		ID:      1,
	}
}

type GetSequencerRpcUrlsParams struct {
	SequencerAddresses []string `json:"sequencer_address_list"`
}

type SequencerRpcUrl struct {
	Address        string `json:"address"`
	ExternalRpcUrl string `json:"external_rpc_url"`
	ClusterRpcUrl  string `json:"cluster_rpc_url"`
}

type GetSequencerRpcUrlsResponse struct {
	SequencerRpcUrls []SequencerRpcUrl `json:"sequencer_rpc_url_list"`
}

type FinalizeBlockMessageParams struct {
	RollupId        string         `json:"rollup_id"`
	ExecutorAddress common.Address `json:"executor_address"`

	PlatformBlockHeight uint64 `json:"platform_block_height"`
	RollupBlockHeight   uint64 `json:"rollup_block_height"`

	BlockCreatorAddress     string `json:"block_creator_address"`
	NextBlockCreatorAddress string `json:"next_block_creator_address"`
}

type FinalizeBlockParams struct {
	Message   FinalizeBlockMessageParams `json:"finalize_block_message"`
	Signature string                     `json:"signature"`
}

type GetRawTransactionsParams struct {
	RollupId          string `json:"rollup_id"`
	RollupBlockHeight uint64 `json:"rollup_block_height"`
}

type GetRawTransactionsResponse struct {
	RawTransactions []string `json:"raw_transaction_list"`
}

var abiString string = `[
		{
      "inputs": [
        {
          "internalType": "string",
          "name": "clusterId",
          "type": "string"
        }
      ],
      "name": "getSequencers",
      "outputs": [
        {
          "internalType": "address[]",
          "name": "",
          "type": "address[]"
        }
      ],
      "stateMutability": "view",
      "type": "function"
    }
	]`
