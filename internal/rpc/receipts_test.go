package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestBlockReceiptsSuccess(t *testing.T) {
	blockHash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	transaction1 := mustHash(t, "0x"+strings.Repeat("11", 32))
	transaction2 := mustHash(t, "0x"+strings.Repeat("22", 32))
	address := mustAddress(t, "0x"+strings.Repeat("aa", 20))
	topic1 := mustHash(t, "0x"+strings.Repeat("01", 32))
	topic2 := mustHash(t, "0x"+strings.Repeat("02", 32))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		if request.Method != "eth_getBlockReceipts" {
			t.Errorf("method = %q, want eth_getBlockReceipts", request.Method)
		}
		if len(request.Params) != 1 || request.Params[0] != blockHash.Hex() {
			t.Errorf("params = %v, want [%q]", request.Params, blockHash.Hex())
		}
		writeResult(t, w, request.ID, []any{
			map[string]any{
				"transactionHash": transaction1.Hex(),
				"logs": []any{
					map[string]any{
						"address":  address.Hex(),
						"topics":   []string{topic1.Hex(), topic2.Hex()},
						"data":     "0xdeadbeef",
						"logIndex": "0x0",
						"removed":  false,
					},
					map[string]any{
						"address":  address.Hex(),
						"topics":   []string{topic1.Hex()},
						"data":     "0x",
						"logIndex": "0x1",
						"removed":  true,
					},
				},
			},
			map[string]any{
				"transactionHash": transaction2.Hex(),
				"logs":            []any{},
			},
		})
	}))
	defer server.Close()

	receipts, err := NewClient(server.URL, Options{}).BlockReceipts(context.Background(), blockHash)
	if err != nil {
		t.Fatalf("BlockReceipts: %v", err)
	}
	if len(receipts) != 2 {
		t.Fatalf("len(receipts) = %d, want 2", len(receipts))
	}
	if receipts[0].TransactionHash != transaction1 {
		t.Fatalf("receipt 0 transaction = %s, want %s", receipts[0].TransactionHash.Hex(), transaction1.Hex())
	}
	if len(receipts[0].Logs) != 2 {
		t.Fatalf("len(receipt 0 logs) = %d, want 2", len(receipts[0].Logs))
	}

	first := receipts[0].Logs[0]
	if first.Address != address {
		t.Fatalf("log 0 address = %s, want %s", first.Address.Hex(), address.Hex())
	}
	if len(first.Topics) != 2 || first.Topics[0] != topic1 || first.Topics[1] != topic2 {
		t.Fatalf("log 0 topics = %v, want [%s %s]", first.Topics, topic1.Hex(), topic2.Hex())
	}
	if !bytes.Equal(first.Data, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("log 0 data = %x, want deadbeef", first.Data)
	}
	if first.LogIndex != 0 || first.Removed {
		t.Fatalf("log 0 index = %d, removed = %v, want 0, false", first.LogIndex, first.Removed)
	}
	if first.TxHash != transaction1 {
		t.Fatalf("log 0 transaction = %s, want the receipt hash %s", first.TxHash.Hex(), transaction1.Hex())
	}

	second := receipts[0].Logs[1]
	if len(second.Data) != 0 {
		t.Fatalf("log 1 data = %x, want empty", second.Data)
	}
	if second.LogIndex != 1 || !second.Removed {
		t.Fatalf("log 1 index = %d, removed = %v, want 1, true", second.LogIndex, second.Removed)
	}

	if receipts[1].TransactionHash != transaction2 {
		t.Fatalf("receipt 1 transaction = %s, want %s", receipts[1].TransactionHash.Hex(), transaction2.Hex())
	}
	if len(receipts[1].Logs) != 0 {
		t.Fatalf("len(receipt 1 logs) = %d, want 0", len(receipts[1].Logs))
	}
}

func TestBlockReceiptsEmpty(t *testing.T) {
	blockHash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	server := resultServer(t, []any{})

	receipts, err := NewClient(server.URL, Options{}).BlockReceipts(context.Background(), blockHash)
	if err != nil {
		t.Fatalf("BlockReceipts: %v", err)
	}
	if len(receipts) != 0 {
		t.Fatalf("len(receipts) = %d, want 0", len(receipts))
	}
}

func TestBlockReceiptsNullResult(t *testing.T) {
	blockHash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	server := resultServer(t, nil)

	_, err := NewClient(server.URL, Options{}).BlockReceipts(context.Background(), blockHash)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want domain.ErrNotFound", err)
	}
}

func TestBlockReceiptsMalformed(t *testing.T) {
	blockHash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	transaction := mustHash(t, "0x"+strings.Repeat("11", 32))
	address := mustAddress(t, "0x"+strings.Repeat("aa", 20))
	topic := mustHash(t, "0x"+strings.Repeat("01", 32))
	validLog := func() map[string]any {
		return map[string]any{
			"address":  address.Hex(),
			"topics":   []string{topic.Hex()},
			"data":     "0x",
			"logIndex": "0x0",
			"removed":  false,
		}
	}

	tests := []struct {
		name     string
		mutate   func(map[string]any)
		wantText string
	}{
		{
			name:     "malformed topic",
			mutate:   func(log map[string]any) { log["topics"] = []string{"0x1234"} },
			wantText: "topic 0",
		},
		{
			name:     "malformed data",
			mutate:   func(log map[string]any) { log["data"] = "0xzz" },
			wantText: "data",
		},
		{
			name:     "odd length data",
			mutate:   func(log map[string]any) { log["data"] = "0xabc" },
			wantText: "data",
		},
		{
			name:     "missing address",
			mutate:   func(log map[string]any) { delete(log, "address") },
			wantText: "address",
		},
		{
			name:     "malformed logIndex",
			mutate:   func(log map[string]any) { log["logIndex"] = "12" },
			wantText: "logIndex",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := validLog()
			test.mutate(log)
			server := resultServer(t, []any{
				map[string]any{"transactionHash": transaction.Hex(), "logs": []any{log}},
			})

			_, err := NewClient(server.URL, Options{}).BlockReceipts(context.Background(), blockHash)
			if err == nil {
				t.Fatal("BlockReceipts succeeded, want error")
			}
			if !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error = %q, want text %q", err, test.wantText)
			}
		})
	}
}

func TestBlockReceiptsMalformedTransactionHash(t *testing.T) {
	blockHash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	server := resultServer(t, []any{
		map[string]any{"transactionHash": "0x1234", "logs": []any{}},
	})

	_, err := NewClient(server.URL, Options{}).BlockReceipts(context.Background(), blockHash)
	if err == nil {
		t.Fatal("BlockReceipts succeeded, want error")
	}
	if !strings.Contains(err.Error(), "transactionHash") {
		t.Fatalf("error = %q, want transactionHash detail", err)
	}
}

func TestBlockReceiptsRPCError(t *testing.T) {
	blockHash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"error":   map[string]any{"code": -32000, "message": "block not found"},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	_, err := NewClient(server.URL, Options{}).BlockReceipts(context.Background(), blockHash)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v (%T), want *RPCError", err, err)
	}
	if rpcErr.Code != -32000 || rpcErr.Message != "block not found" {
		t.Fatalf("RPCError = %+v, want code -32000 with server message", rpcErr)
	}
}
