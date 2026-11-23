//go:build opnative && contracts

package remote

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/paperreport"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func TestPaperDelayedExecutionKeepsTransactionWhilePrefixChangesOutcome(t *testing.T) {
	f := newOPFixture(t, "jovian")
	full, calls := nativeFlashPrefix(t, f)
	// The first two transactions leave the opportunity executable. The third
	// changes the pool callback behavior and makes the very same call revert.
	prefixes := make([]flashPrefix, 2)
	if err := f.engine.request(map[string]any{"method": "checkpoint"}, nil); err != nil {
		t.Fatal(err)
	}
	gas := uint64(0)
	for i, call := range calls[:3] {
		result, err := f.executeAt(call, nil, true, true, map[string]any{"number": "0xb", "time": "0x3ea"})
		if err != nil {
			t.Fatal(err)
		}
		used, err := quantity(result.GasUsed)
		if err != nil {
			t.Fatal(err)
		}
		gas += used
		if i < 1 {
			continue
		}
		prefix := full.freeze()
		prefix.Transactions = prefix.Transactions[:i+1]
		prefix.Version.TxIndex = uint64(i)
		prefix.Version.PrefixHash = domain.Hash{byte(i)}
		prefix.Index = uint64(i)
		usedTotal := gas
		prefix.GasUsed = &usedTotal
		prefixes[i-1] = prefix
	}
	if err := f.engine.request(map[string]any{"method": "restore"}, nil); err != nil {
		t.Fatal(err)
	}
	node := &flashRPC{base: &opRPC{fixture: f, replay: true, paper: true}}
	client := node.server(t)
	parent, err := client.ExecutionBlock(t.Context(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	request := f.request(t)
	keys := []domain.PoolKey{request.Plan.Steps[0].Pool, request.Plan.Steps[1].Pool}
	store := poolstate.New()
	for _, key := range keys {
		if err := store.Upsert(pool.State{Key: key, Token0: f.token0, Token1: f.token1, Fee: 3000, TickSpacing: 60, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1000000)}); err != nil {
			t.Fatal(err)
		}
	}
	// Net only feeds the fee preflight, which must clear the fixture's ~3.4e10 wei
	// fee; the executor's minimums come from the legs and its floor is 1 wei.
	opportunity := arb.Opportunity{Cycle: graph.Cycle{Start: f.token0, Pools: keys, Len: 2}, Input: big.NewInt(1000), Output: big.NewInt(1200), Net: big.NewInt(1e12), Legs: []arb.Leg{{Pool: keys[0], AmountIn: big.NewInt(1000), AmountOut: big.NewInt(2000)}, {Pool: keys[1], AmountIn: big.NewInt(2000), AmountOut: big.NewInt(1200)}}}
	artifact := f.artifacts["ArbitrageExecutor"]
	p := &executionPaper{client: client, chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, artifact: paperArtifact{abi: artifact.abi, code: artifact.code, hash: digest(artifact.code)}, policy: executionPaperPolicy{WrappedNative: f.token0.Hex(), GasLimit: 2000000, TipWei: "0", TipScenariosWei: []string{"0", "1", "1000000"}, SlippageBPS: 50, DelaysMS: []int64{200}}, jobs: make(chan paperExecutionJob, 2)}
	p.source.unsupported.Store(true)
	now := time.Now()
	p.schedule(paperExecutionJob{opportunity: opportunity, discovered: now, anchor: parent, prefix: &prefixes[0], event: arb.SearchEvent{Job: arb.SearchJob{View: store.View(), Version: prefixes[0].Version, ReceivedAt: now}}})
	baseline := <-p.jobs
	first := p.evaluate(t.Context(), baseline)
	later := p.delayJob(p.pending)
	later.prefix = &prefixes[1]
	later.executionVersion = prefixes[1].Version
	later.observed = now.Add(250 * time.Millisecond)
	second := p.evaluate(t.Context(), later)
	if first.Status != "success" || second.Status != "reverted" || second.GasUsed == 0 || second.TotalFee == nil {
		t.Fatalf("baseline=%+v arrival=%+v", first, second)
	}
	if first.Bidding == nil || first.Bidding.Status == "unknown" || first.Bidding.Profit == nil || first.Bidding.Profit.Cmp(first.Profit) != 0 || first.Bidding.FeeUpperBound.Cmp(first.TotalFee) < 0 || len(first.Bidding.Scenarios) != 3 {
		t.Fatalf("native execution did not produce a conservative bid budget: %+v", first.Bidding)
	}
	if second.Bidding == nil || second.Bidding.Status != "reverted" || second.Bidding.MaxPositiveTip != nil {
		t.Fatal("revert acquired a positive bid budget", second.Bidding)
	}
	if first.Group != second.Group || first.ID == second.ID || !reflect.DeepEqual(first.Call, second.Call) || second.StateBasis != "later_observed_prefix" || second.ActualDelayNS != 250000000 {
		t.Fatal("delay changed transaction or provenance", first, second)
	}
	var journal bytes.Buffer
	encoder := json.NewEncoder(&journal)
	for _, result := range []paperExecutionResult{{Type: "session", Schema: 1, Basis: "executor_simulation", Chain: p.chain, Artifact: p.artifact.hash}, first, second} {
		if err := encoder.Encode(result); err != nil {
			t.Fatal(err)
		}
	}
	report, err := paperreport.Read(t.Context(), &journal, 10)
	if err != nil || report.Samples != 2 || len(report.Buckets) != 2 || report.Buckets[0].Status["success"] != 1 || report.Buckets[1].Status["reverted"] != 1 || report.Buckets[1].ActualDelay.P50 != 250000000 {
		t.Fatal("native samples lost in report", report, err)
	}

	// Reconstruct these same positions from a full canonical block whose suffix
	// later resets the pool. Applying that suffix would turn the revert into a
	// false success, so compare both outcomes against independent prefix replay.
	raw, _, err := full.rawBlock()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(flashHex(raw))
	options := json.RawMessage(`{"tracer":"muxTracer","tracerConfig":{"callTracer":{},"prestateTracer":{}}}`)
	native, err := node.respond(t.Context(), "debug_traceBlock", []json.RawMessage{encoded, options})
	if err != nil {
		t.Fatal(err)
	}
	records := native.([]map[string]any)
	var traceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var output any
		var failure error
		if req.Method == "debug_traceBlockByNumber" {
			traceCalls.Add(1)
			var selected struct{ Tracer string }
			json.Unmarshal(req.Params[1], &selected)
			var traces []map[string]any
			for _, entry := range records {
				traces = append(traces, map[string]any{"txHash": entry["txHash"], "result": entry["result"].(map[string]any)[selected.Tracer]})
			}
			output = traces
		} else if req.Method == "eth_getBlockByNumber" && string(req.Params[0]) == `"0xb"` {
			txs := make([]string, len(full.Transactions))
			for i, tx := range full.Transactions {
				txs[i] = tx.Hash.Hex()
			}
			output = map[string]any{"number": "0xb", "hash": domain.Hash{11}.Hex(), "parentHash": domain.Hash{10}.Hex(), "timestamp": "0x3ea", "gasLimit": "0x1e84800", "baseFeePerGas": "0x1", "miner": domain.Address{}.Hex(), "mixHash": domain.Hash{}.Hex(), "parentBeaconBlockRoot": domain.Hash{}.Hex(), "extraData": "0x", "transactions": txs}
		} else {
			output, failure = node.respond(r.Context(), req.Method, req.Params)
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": output}
		if failure != nil {
			delete(response, "result")
			response["error"] = map[string]any{"code": -32000, "message": failure.Error()}
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	p.client = rpc.NewClient(server.URL, rpc.Options{})
	p.policy.PrefixSource = "canonical-trace"
	for i, job := range []paperExecutionJob{baseline, later} {
		got := p.evaluate(t.Context(), job)
		want := []paperExecutionResult{first, second}[i]
		if got.Status != want.Status || got.GasUsed != want.GasUsed || got.StateBasis != want.StateBasis || got.CanonicalTraceBlock == nil || got.Flash.StateSource != "canonical-block-traces" || !reflect.DeepEqual(got.Call, want.Call) {
			t.Fatalf("canonical prefix %d differs: %+v", i, got)
		}
	}
	if traceCalls.Load() != 2 {
		t.Fatal("full block traced again for later scenario", traceCalls.Load())
	}

}
