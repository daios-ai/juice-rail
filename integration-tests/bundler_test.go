//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/daios-ai/juice-rail/go/erc4337"
)

// shim is a bundler for the stories: it serves the standard submission RPC and
// executes operations through the EntryPoint. The rail speaks only production
// bundler RPC to it, while the story keeps control of what actually lands.
type shim struct {
	t      *testing.T
	h      *harness
	url    string
	server *httptest.Server

	mu       sync.Mutex
	drop     bool // accept operations and discard them
	accepted int
}

func newShim(t *testing.T, h *harness) *shim {
	t.Helper()
	s := &shim{t: t, h: h}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	s.url = s.server.URL
	t.Cleanup(s.server.Close)
	return s
}

// dropOperations makes the bundler accept and discard, which is how a story
// reaches the state where an operation was submitted but can never land.
func (s *shim) dropOperations(drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop = drop
}

func (s *shim) submissions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

type rpcRequest struct {
	ID     int               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (s *shim) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeRPCError(w, 0, "malformed request")
		return
	}
	if req.Method != "eth_sendUserOperation" {
		writeRPCError(w, req.ID, "unsupported method "+req.Method)
		return
	}

	var op erc4337.UserOperation
	if err := json.Unmarshal(req.Params[0], &op); err != nil {
		writeRPCError(w, req.ID, "malformed operation: "+err.Error())
		return
	}
	var entryPoint common.Address
	if err := json.Unmarshal(req.Params[1], &entryPoint); err != nil {
		writeRPCError(w, req.ID, "malformed entry point")
		return
	}

	s.mu.Lock()
	s.accepted++
	drop := s.drop
	s.mu.Unlock()

	hash := op.Hash(entryPoint, s.h.chainID)
	if !drop {
		if err := s.execute(&op, entryPoint); err != nil {
			writeRPCError(w, req.ID, err.Error())
			return
		}
	}
	// Acceptance is not confirmation: the hash is all a bundler owes us.
	writeRPCResult(w, req.ID, hash)
}

// execute submits the operation through the EntryPoint. The transaction is
// left pending; the story decides when it is mined.
func (s *shim) execute(op *erc4337.UserOperation, entryPoint common.Address) error {
	packed := op.Pack()
	data, err := entryPointABI.Pack("handleOps",
		[]struct {
			Sender             common.Address `json:"sender"`
			Nonce              *big.Int       `json:"nonce"`
			InitCode           []byte         `json:"initCode"`
			CallData           []byte         `json:"callData"`
			AccountGasLimits   [32]byte       `json:"accountGasLimits"`
			PreVerificationGas *big.Int       `json:"preVerificationGas"`
			GasFees            [32]byte       `json:"gasFees"`
			PaymasterAndData   []byte         `json:"paymasterAndData"`
			Signature          []byte         `json:"signature"`
		}{{
			Sender: packed.Sender, Nonce: packed.Nonce, InitCode: packed.InitCode,
			CallData: packed.CallData, AccountGasLimits: packed.AccountGasLimits,
			PreVerificationGas: packed.PreVerificationGas, GasFees: packed.GasFees,
			PaymasterAndData: packed.PaymasterAndData, Signature: packed.Signature,
		}},
		s.h.beneficiary(),
	)
	if err != nil {
		return err
	}
	s.h.sendTxAsync(&entryPoint, data)
	return nil
}

func writeRPCResult(w http.ResponseWriter, id int, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32000, "message": message},
	})
}
