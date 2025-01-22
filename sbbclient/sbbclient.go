package sbbclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/http"
	"time"
)

// Client defines typed wrappers for the SBB RPC API.
type SbbClient struct {
	httpClient *http.Client
}

func New() *SbbClient {
	return &SbbClient{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy:             http.ProxyFromEnvironment,
				DisableKeepAlives: true,
			},
		},
	}
}

func makeRequest(ctx context.Context, url string, body interface{}) (*http.Request, error) {
	b, err := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(b))
	if err != nil {
		return nil, err
	}

	// 헤더 설정 (Cache-Control: no-cache 추가)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache") // 캐시 방지
	return req, nil
}

func (sc *SbbClient) Send(ctx context.Context, url string, body interface{}, result any) error {
	req, err := makeRequest(ctx, url, body)
	if err != nil {
		return err
	}

	res, err := sc.httpClient.Do(req)
	if err != nil {
		return err
	}
	// 해야하는지 확인 필요
	defer func() {
		if err = res.Body.Close(); err != nil {

		}
	}()

	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	var jsonRpcResponse JSONRPCResponse
	if err = json.Unmarshal(resBody, &jsonRpcResponse); err != nil {
		return err
	}
	if jsonRpcResponse.Error != nil {
		return errors.New(jsonRpcResponse.Error.Message)
	}
	if jsonRpcResponse.Result != nil && result != nil {
		if err = json.Unmarshal(jsonRpcResponse.Result, result); err != nil {
			return err
		}
	}
	return nil
}

// JSON-RPC 응답 형식 정의
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      int             `json:"id"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}
