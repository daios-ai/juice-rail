package erc4337

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Bundler submits operations to an ERC-4337 bundler.
//
// Acceptance is not confirmation, and nothing downstream reads a bundler's
// opinion of an outcome, so submission is the only method the rail needs.
type Bundler interface {
	SendUserOperation(ctx context.Context, op *UserOperation, entryPoint common.Address) (common.Hash, error)
}

// HTTPBundler speaks the standard bundler JSON-RPC.
type HTTPBundler struct {
	URL    string
	Client *http.Client
}

func NewHTTPBundler(url string) *HTTPBundler {
	return &HTTPBundler{URL: url, Client: &http.Client{Timeout: 30 * time.Second}}
}

type rpcRequest struct {
	Version string            `json:"jsonrpc"`
	ID      int               `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type rpcResponse struct {
	Version string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("bundler error %d: %s", e.Code, e.Message) }

// SendUserOperation submits an operation and returns the hash the bundler
// acknowledges. Submitting the same operation again is safe: the EntryPoint
// nonce and the rail's own binding both keep it at most once.
func (b *HTTPBundler) SendUserOperation(ctx context.Context, op *UserOperation, entryPoint common.Address) (common.Hash, error) {
	opJSON, err := json.Marshal(op)
	if err != nil {
		return common.Hash{}, err
	}
	epJSON, err := json.Marshal(entryPoint)
	if err != nil {
		return common.Hash{}, err
	}

	var hash common.Hash
	if err := b.call(ctx, "eth_sendUserOperation", []json.RawMessage{opJSON, epJSON}, &hash); err != nil {
		return common.Hash{}, err
	}
	return hash, nil
}

func (b *HTTPBundler) call(ctx context.Context, method string, params []json.RawMessage, out any) error {
	body, err := json.Marshal(rpcRequest{Version: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: http %d: %s", method, resp.StatusCode, bytes.TrimSpace(raw))
	}

	var r rpcResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("%s: decode: %w", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %w", method, r.Error)
	}
	return json.Unmarshal(r.Result, out)
}
