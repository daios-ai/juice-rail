package erc4337

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestSendUserOperationSpeaksTheStandardRPC(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x`+strings.Repeat("11", 32)+`"}`)
	}))
	defer server.Close()

	entryPoint := common.HexToAddress("0x0000000000000000000000000000000000002001")
	hash, err := NewHTTPBundler(server.URL).SendUserOperation(context.Background(), goldenOp(), entryPoint)
	if err != nil {
		t.Fatal(err)
	}
	if hash != common.HexToHash("0x"+strings.Repeat("11", 32)) {
		t.Fatalf("hash = %s", hash)
	}

	if body["method"] != "eth_sendUserOperation" {
		t.Errorf("method = %v", body["method"])
	}
	params, ok := body["params"].([]any)
	if !ok || len(params) != 2 {
		t.Fatalf("params = %v", body["params"])
	}
	op, ok := params[0].(map[string]any)
	if !ok || op["sender"] == nil || op["callData"] == nil {
		t.Fatalf("first parameter must be the operation, got %v", params[0])
	}
	if !strings.EqualFold(params[1].(string), entryPoint.Hex()) {
		t.Errorf("second parameter must be the entry point, got %v", params[1])
	}
}

func TestSendUserOperationReportsBundlerErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32521,"message":"execution reverted"}}`)
	}))
	defer server.Close()

	_, err := NewHTTPBundler(server.URL).SendUserOperation(context.Background(), goldenOp(), common.Address{})
	if err == nil || !strings.Contains(err.Error(), "execution reverted") {
		t.Fatalf("want the bundler's reason, got %v", err)
	}
}

func TestSendUserOperationReportsTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream down")
	}))
	defer server.Close()

	_, err := NewHTTPBundler(server.URL).SendUserOperation(context.Background(), goldenOp(), common.Address{})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want an HTTP status in the error, got %v", err)
	}
}

func TestSendUserOperationHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewHTTPBundler("http://127.0.0.1:1").SendUserOperation(ctx, goldenOp(), common.Address{})
	if err == nil {
		t.Fatal("want an error from a cancelled context")
	}
}
