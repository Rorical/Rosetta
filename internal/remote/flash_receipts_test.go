package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func receiptDocument(hash domain.Hash, index uint64) map[string]any {
	return map[string]any{"transactionHash": hash.Hex(), "blockNumber": "0xb", "transactionIndex": fmt.Sprintf("0x%x", index), "status": "0x1", "logs": []any{map[string]any{"address": addressAt(5).Hex(), "topics": []string{(domain.Hash{9}).Hex()}, "data": "0x1234", "transactionHash": hash.Hex(), "blockNumber": "0xb", "transactionIndex": fmt.Sprintf("0x%x", index), "removed": false}}}
}
func TestFlashblocksReceiptCompletionWaitsForEveryTransaction(t *testing.T) {
	hashes := []domain.Hash{{1}, {2}}
	calls := make(map[string]int)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requests []struct {
			ID     uint64
			Method string
			Params []string
		}
		if err := json.NewDecoder(r.Body).Decode(&requests); err != nil {
			t.Error(err)
			return
		}
		results := make([]any, len(requests))
		mu.Lock()
		defer mu.Unlock()
		for i, request := range requests {
			if request.Method != "eth_getTransactionReceipt" || len(request.Params) != 1 {
				t.Error("wrong receipt RPC")
				return
			}
			hash, err := domain.ParseHash(request.Params[0])
			if err != nil {
				t.Error(err)
				return
			}
			calls[request.Params[0]]++
			var value any = receiptDocument(hash, 3)
			if hash == hashes[0] && calls[request.Params[0]] == 1 {
				value = nil
			}
			if hash == hashes[1] {
				value = receiptDocument(hash, 4)
			}
			results[i] = map[string]any{"id": request.ID, "jsonrpc": "2.0", "result": value}
		}
		json.NewEncoder(w).Encode(results)
	}))
	defer server.Close()
	block := feed.Flashblock{Number: 11, MissingReceipts: true, Transactions: []feed.FlashTransaction{{Hash: hashes[0]}, {Hash: hashes[1]}}}
	complete, err := completeFlashReceipts(t.Context(), rpc.NewClient(server.URL, rpc.Options{}), block, 3)
	if err != nil || complete.MissingReceipts || len(complete.Transactions[0].Logs) != 1 || !complete.Transactions[1].Successful {
		t.Fatal(complete, err)
	}
	if block.Transactions[0].Successful || block.Transactions[0].Logs != nil {
		t.Fatal("partial completion mutated input frame")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls[hashes[0].Hex()] != 2 || calls[hashes[1].Hex()] != 1 {
		t.Fatal("completed receipts were refetched", calls)
	}
}
func TestFlashblocksRPCReceiptRejectsWrongIdentityAndIncompleteEffects(t *testing.T) {
	for _, fault := range []string{"transaction", "height", "index", "status", "missing logs", "removed", "wrong log", "missing topics"} {
		t.Run(fault, func(t *testing.T) {
			hash := domain.Hash{1}
			receipt := receiptDocument(hash, 2)
			log := receipt["logs"].([]any)[0].(map[string]any)
			switch fault {
			case "transaction":
				receipt["transactionHash"] = (domain.Hash{2}).Hex()
			case "height":
				receipt["blockNumber"] = "0xc"
			case "index":
				receipt["transactionIndex"] = "0x3"
			case "status":
				receipt["status"] = "0x0"
			case "missing logs":
				delete(receipt, "logs")
			case "removed":
				log["removed"] = true
			case "wrong log":
				log["transactionHash"] = (domain.Hash{2}).Hex()
			case "missing topics":
				delete(log, "topics")
			}
			raw, _ := json.Marshal(receipt)
			if _, _, err := decodeFlashRPCReceipt(raw, 11, 2, hash); err == nil {
				t.Fatal("bad pending receipt accepted")
			}
		})
	}
}
func TestFlashblocksMissingReceiptDeadlineAndEarlyRetirement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requests []struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&requests)
		results := make([]any, len(requests))
		for i, request := range requests {
			results[i] = map[string]any{"id": request.ID, "jsonrpc": "2.0", "result": nil}
		}
		json.NewEncoder(w).Encode(results)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	block := feed.Flashblock{Number: 11, MissingReceipts: true, Transactions: []feed.FlashTransaction{{Hash: domain.Hash{1}}}}
	if _, err := completeFlashReceipts(ctx, rpc.NewClient(server.URL, rpc.Options{}), block, 0); err == nil {
		t.Fatal("missing receipts accepted")
	}
	runtime := &runtime{tentative: domain.StateVersion{Source: domain.SourceFast, PrefixHash: domain.Hash{1}}, flash: flashCursor{active: true, index: 2}}
	runtime.retireFlashResults()
	if runtime.tentative != (domain.StateVersion{}) || !runtime.flash.active || runtime.flash.index != 2 {
		t.Fatal("receipt I/O did not retire results while preserving the prefix")
	}
}
