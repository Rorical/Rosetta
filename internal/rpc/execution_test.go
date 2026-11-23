package rpc

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func executionServer(t *testing.T, result any) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct{ ID uint64 }
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return NewClient(server.URL, Options{})
}

func executionHeader() map[string]any {
	return map[string]any{"number": "0xa", "hash": domain.Hash{10}.Hex(), "parentHash": domain.Hash{9}.Hex(), "timestamp": "0x64", "baseFeePerGas": "0x1", "gasLimit": "0x100000", "miner": domain.Address{}.Hex(), "transactions": []string{domain.Hash{20}.Hex()}}
}

func TestExecutionBlockBindsRequestedPosition(t *testing.T) {
	for _, tag := range []any{domain.Hash{11}, "0xb"} {
		t.Run("wrong position", func(t *testing.T) {
			if _, err := executionServer(t, executionHeader()).ExecutionBlock(t.Context(), tag); err == nil {
				t.Fatal("accepted another execution position")
			}
		})
	}
	block, err := executionServer(t, executionHeader()).ExecutionBlock(t.Context(), "latest")
	if err != nil || block.Ref.Number != 10 || block.BaseFee.Cmp(big.NewInt(1)) != 0 {
		t.Fatal(block, err)
	}
}

func TestExecutionReceiptsRejectIncompleteAndDetachedEffects(t *testing.T) {
	block := ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, Transactions: []domain.Hash{{20}}}
	receipt := func() map[string]any {
		return map[string]any{"transactionHash": domain.Hash{20}.Hex(), "blockHash": block.Ref.Hash.Hex(), "blockNumber": "0xa", "transactionIndex": "0x0", "status": "0x1", "logs": []any{map[string]any{"address": domain.Address{1}.Hex(), "topics": []string{}, "data": "0x", "logIndex": "0x0", "blockNumber": "0xa"}}}
	}
	tests := []struct {
		name   string
		change func(map[string]any)
	}{
		{"missing logs", func(r map[string]any) { delete(r, "logs") }},
		{"wrong transaction", func(r map[string]any) { r["transactionHash"] = domain.Hash{21}.Hex() }},
		{"wrong index", func(r map[string]any) { r["transactionIndex"] = "0x1" }},
		{"revert logs", func(r map[string]any) { r["status"] = "0x0" }},
		{"wrong log height", func(r map[string]any) { r["logs"].([]any)[0].(map[string]any)["blockNumber"] = "0xb" }},
		{"removed log", func(r map[string]any) { r["logs"].([]any)[0].(map[string]any)["removed"] = true }},
		{"too many topics", func(r map[string]any) {
			r["logs"].([]any)[0].(map[string]any)["topics"] = []string{domain.Hash{}.Hex(), domain.Hash{}.Hex(), domain.Hash{}.Hex(), domain.Hash{}.Hex(), domain.Hash{}.Hex()}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := receipt()
			test.change(r)
			if _, err := executionServer(t, []any{r}).ExecutionReceipts(t.Context(), block); err == nil {
				t.Fatal("accepted incomplete or detached effects")
			}
		})
	}
	for _, result := range []any{nil, []any{}} {
		if _, err := executionServer(t, result).ExecutionReceipts(t.Context(), block); err == nil {
			t.Fatal("accepted missing receipt")
		}
	}
	valid, err := executionServer(t, []any{receipt()}).ExecutionReceipts(t.Context(), block)
	if err != nil || len(valid) != 1 || len(valid[0].Logs) != 1 {
		t.Fatal(valid, err)
	}
}
