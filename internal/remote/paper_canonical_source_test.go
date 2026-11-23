package remote

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func canonicalPaperServer(t *testing.T, fault string) (*rpc.Client, *atomic.Int32, flashPrefix) {
	t.Helper()
	count := new(atomic.Int32)
	parent, hash, tx := domain.Hash{10}, domain.Hash{11}, domain.Hash{1}
	prefix := flashPrefix{Base: feed.FlashblockBase{Parent: parent, Number: 11, Timestamp: 1002, Execution: &feed.FlashExecution{GasLimit: 32000000, BaseFee: big.NewInt(1)}}, Transactions: []feed.FlashTransaction{{Hash: tx}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch request.Method {
		case "eth_getBlockByNumber":
			selected := hash
			if fault == "reorg" && count.Load() > 0 {
				selected = domain.Hash{99}
			}
			result = map[string]any{"hash": selected.Hex(), "number": "0xb", "parentHash": parent.Hex(), "timestamp": "0x3ea", "gasLimit": "0x1e84800", "baseFeePerGas": "0x1", "miner": domain.Address{}.Hex(), "mixHash": domain.Hash{}.Hex(), "parentBeaconBlockRoot": domain.Hash{}.Hex(), "extraData": "0x", "transactions": []string{tx.Hex()}}
		case "debug_traceBlockByNumber":
			count.Add(1)
			hashText := tx.Hex()
			if fault == "transaction" {
				hashText = domain.Hash{9}.Hex()
			}
			var options struct{ Tracer string }
			json.Unmarshal(request.Params[1], &options)
			body := any(map[string]any{"gasUsed": "0x5208"})
			if options.Tracer == "prestateTracer" {
				body = map[string]any{"pre": map[string]any{}, "post": map[string]any{}}
			}
			result = []any{map[string]any{"txHash": hashText, "result": body}}
			if fault == "short" {
				result = []any{}
			}
		default:
			t.Error("unexpected", request.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return rpc.NewClient(server.URL, rpc.Options{}), count, prefix
}
func TestPaperCanonicalTraceCacheAndIdentity(t *testing.T) {
	for _, fault := range []string{"", "transaction", "short", "reorg"} {
		t.Run(fault, func(t *testing.T) {
			client, calls, prefix := canonicalPaperServer(t, fault)
			var source paperCanonicalSource
			err := source.load(t.Context(), client, 11)
			if fault != "" {
				if err == nil {
					t.Fatal("accepted", fault)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := source.matches(prefix); err != nil {
				t.Fatal(err)
			}
			if err := source.load(t.Context(), client, 11); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatal("same block traced repeatedly", calls.Load())
			}
			prefix.Transactions[0].Hash = domain.Hash{88}
			if source.matches(prefix) == nil {
				t.Fatal("noncanonical prefix accepted")
			}
			prefix.Transactions = nil
			prefix.Base.Execution.BaseFee = big.NewInt(2)
			if source.matches(prefix) == nil {
				t.Fatal("changed execution context accepted")
			}
		})
	}
}
func TestPaperFeePrefetchSharesOracleReadsAcrossVirtualSetup(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests.Add(1)
		value := "0x6000"
		if request.Method == "eth_call" {
			value = fmt.Sprintf("0x%064x", 1)
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": value})
	}))
	defer server.Close()
	s := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: domain.BlockRef{Hash: domain.Hash{1}}}, overlay: map[string]any{}}
	task := s.startPaperFeePrefetch(t.Context(), 2000000)
	// Changing only virtual registry state must not make identical fee reads miss.
	s.overlay = map[string]any{domain.Address{9}.Hex(): map[string]any{"balance": "0x1"}}
	if result := <-task; result.err != nil {
		t.Fatal(result.err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.opFeeWord(t.Context(), "getOperatorFee(uint256)", 2000000); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 4 {
		t.Fatal("prefetched oracle reads repeated", requests.Load())
	}
	// A different snapshot must fetch its own fee state.
	other := &snapshot{client: s.client, block: rpc.ExecutionBlock{Ref: domain.BlockRef{Hash: domain.Hash{2}}}}
	if _, err := other.opFeeWord(t.Context(), "getOperatorFee(uint256)", 2000000); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 5 {
		t.Fatal("cross-state fee reuse")
	}
}

func TestPaperCompetitionUsesOnlyLaterSuccessfulPoolActivity(t *testing.T) {
	key := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{5}}
	log := fmt.Sprintf(`{"gasUsed":"0x5208","from":"%s","logs":[{"address":"%s","topics":[],"data":"0x","position":0}]}`, domain.Address{6}.Hex(), key.Address.Hex())
	var touching flashCallFrame
	if err := json.Unmarshal([]byte(log), &touching); err != nil {
		t.Fatal(err)
	}
	reverted := touching
	reverted.Error = "reverted"
	source := paperCanonicalSource{block: paperCanonicalBlock{header: &flashHeader{BaseFee: big.NewInt(2)}, transactions: []domain.Hash{{1}, {2}, {3}}}, calls: []paperCallTrace{{Result: touching}, {Result: touching}, {Result: reverted}}, receipts: []rpc.ExecutionReceipt{{}, {Fees: &domain.ReceiptFees{GasPrice: big.NewInt(5), GasUsed: 21000, Total: big.NewInt(123)}}, {}}}
	prefix := flashPrefix{Transactions: []feed.FlashTransaction{{Hash: domain.Hash{1}}}}
	opportunity := arb.Opportunity{Cycle: graph.Cycle{Pools: []domain.PoolKey{key}}}
	result := source.competition(t.Context(), nil, prefix, opportunity)
	if result.Error != "" || len(result.Transactions) != 1 || result.Transactions[0].Index != 1 || result.Transactions[0].PriorityFee.Int64() != 3 || result.Transactions[0].Fees.Total.Int64() != 123 {
		t.Fatalf("wrong competition sample: %+v", result)
	}
}
