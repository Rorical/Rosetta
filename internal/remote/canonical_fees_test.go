package remote

import (
	"encoding/hex"
	"encoding/json"
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestCanonicalFeeReadsShareHashBoundSearchCache(t *testing.T) {
	var calls atomic.Int32
	hash := domain.Hash{10}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error("expected fee batch", err)
			w.WriteHeader(500)
			return
		}
		var rows []any
		for _, call := range batch {
			calls.Add(1)
			var block struct {
				BlockHash        string
				RequireCanonical bool
			}
			json.Unmarshal(call.Params[1], &block)
			if call.Method != "eth_call" || block.BlockHash != hash.Hex() || !block.RequireCanonical {
				t.Error("fee read not pinned to canonical hash", call)
			}
			rows = append(rows, map[string]any{"id": call.ID, "result": "0x" + hex.EncodeToString(wordData(393))})
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	cfg, err := (config.Config{NetworkName: "optimism"}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	spec := bootstrap.Spec{Address: domain.Address{1}, Kind: domain.PoolKindSlipstream, Token0: domain.Address{2}, Token1: domain.Address{3}, Fee: 500, TickSpacing: 10}
	block := rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: hash}}
	client := rpc.NewClient(server.URL, rpc.Options{})
	fees, cache, err := canonicalFees(t.Context(), cfg, client, block, []bootstrap.Spec{spec})
	if err != nil {
		t.Fatal(err)
	}
	if len(fees) != 1 || fees[0].Fee != 393 {
		t.Fatal(fees)
	}
	reader := &snapshot{client: client, block: block}
	reader.reads.Store(cache)
	key := domain.PoolKey{Address: spec.Address, Kind: spec.Kind}
	base := state.New()
	base.Upsert(pool.State{Key: key, Token0: spec.Token0, Token1: spec.Token1, TickSpacing: 10, Fee: 500})
	// Canonical search uses the already-fetched fee even though its original
	// shallow snapshot still contains the previous value.
	table, err := graph.BuildTableFromCycles([]graph.Cycle{{Start: spec.Token0, Pools: []domain.PoolKey{key, {Address: domain.Address{4}, Kind: domain.PoolKindV2}}, Len: 2}})
	if err != nil {
		t.Fatal(err)
	}
	view, issues, err := prepareDependencies(reader, table, 1, false)(t.Context(), base.View(), []domain.PoolKey{key})
	if err != nil || len(issues) != 0 {
		t.Fatal(issues, err)
	}
	p, _ := view.Pool(key)
	if p.Fee != 393 || calls.Load() != 1 {
		t.Fatal("fee repeated or not applied", p.Fee, calls.Load())
	}
}

func TestObservedFeesDirtyOnlyChangedOrNewlyEligiblePools(t *testing.T) {
	for _, mode := range []string{"unchanged", "changed", "new evidence"} {
		t.Run(mode, func(t *testing.T) {
			store, pools := poolFixture(t)
			topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 8, Notional: func(domain.Address) *big.Int { return big.NewInt(100) }})
			if err != nil {
				t.Fatal(err)
			}
			topology.DynamicPools = []domain.PoolKey{pools[0].Key}
			fast, err := ingest.NewFastBranch(store, ingest.FastOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sink := &captureSearch{}
			r := &runtime{store: store, fast: fast, worker: &arb.Worker{}, topology: topology, search: pipeline.NewSearchScheduler(sink, 1, nil), waitingFees: map[domain.PoolKey]bool{}}
			fee := pools[0]
			if mode == "changed" {
				fee.Fee++
			}
			if mode == "new evidence" {
				r.waitingFees[fee.Key] = true
			}
			next := store.Block()
			next.Number++
			next.ParentHash = store.Block().Hash
			next.Hash = domain.Hash{11}
			if err = r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: next}, fees: []pool.State{fee}}); err != nil {
				t.Fatal(err)
			}
			if len(sink.jobs) != 1 {
				t.Fatal("search frame missing")
			}
			dirty := sink.jobs[0].Dirty
			if mode == "unchanged" && len(dirty) != 0 {
				t.Fatal("an unchanged observed fee dirtied its routes", dirty)
			}
			if mode != "unchanged" && (len(dirty) != 1 || dirty[0] != fee.Key) {
				t.Fatal("a fee change or eligibility transition was lost", dirty)
			}
		})
	}
}

// The follower's fee schedule reads scheduled fees only when due, so
// most blocks carry no fee observations. That is not a backend without fee
// observation: rescreening every V4 pool then re-ranks nearly every cycle on
// each block (hundreds of thousands of candidates). A scheduled fee that
// was not due is unchanged; a Slipstream fee moves without being read and
// keeps its rescreen, as does every pool when fees were never scheduled.
func TestScheduledFeesWithNothingDueDoNotRescreen(t *testing.T) {
	v4 := domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{0x44}, PoolID: domain.Hash{1}}
	slipstream := domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{0x55}}
	for scheduled, want := range map[bool][]domain.PoolKey{true: {slipstream}, false: {v4, slipstream}} {
		store, pools := poolFixture(t)
		topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 8, Notional: func(domain.Address) *big.Int { return big.NewInt(100) }})
		if err != nil {
			t.Fatal(err)
		}
		topology.DynamicPools = []domain.PoolKey{v4, slipstream}
		fast, err := ingest.NewFastBranch(store, ingest.FastOptions{})
		if err != nil {
			t.Fatal(err)
		}
		sink := &captureSearch{}
		r := &runtime{store: store, fast: fast, worker: &arb.Worker{}, topology: topology, search: pipeline.NewSearchScheduler(sink, 1, nil), waitingFees: map[domain.PoolKey]bool{}}
		next := store.Block()
		next.Number++
		next.ParentHash = store.Block().Hash
		next.Hash = domain.Hash{11}
		if err = r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: next}, feesScheduled: scheduled}); err != nil {
			t.Fatal(err)
		}
		if len(sink.jobs) != 1 {
			t.Fatal("search frame missing")
		}
		dirty := slices.Clone(sink.jobs[0].Dirty)
		slices.SortFunc(dirty, domain.ComparePoolKeys)
		slices.SortFunc(want, domain.ComparePoolKeys)
		if !slices.Equal(dirty, want) {
			t.Fatalf("scheduled %v: dirty %v, want %v", scheduled, dirty, want)
		}
	}
}

// Coarse preparation reloaded every dynamic fee over RPC, one call after
// another, including hookless V4 fees that only a setter changes and the
// follower's schedule keeps current: about 21 ms of a typical 51 ms search.
// Scheduled fees come from the store.
func TestPrepareDoesNotRereadScheduledFees(t *testing.T) {
	stateView := domain.Address{5}
	var stateViewCalls atomic.Int32
	answer := func(call json.RawMessage) map[string]any {
		var request struct {
			ID     uint64
			Params []json.RawMessage
		}
		json.Unmarshal(call, &request)
		var target struct{ To string }
		if len(request.Params) > 0 {
			json.Unmarshal(request.Params[0], &target)
		}
		if target.To == stateView.Hex() {
			stateViewCalls.Add(1)
		}
		return map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": "0x" + hex.EncodeToString(wordData(393))}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		var batch []json.RawMessage
		if json.Unmarshal(raw, &batch) != nil {
			json.NewEncoder(w).Encode(answer(raw))
			return
		}
		var rows []any
		for _, call := range batch {
			rows = append(rows, answer(call))
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	a, b := domain.Address{2}, domain.Address{3}
	manager := domain.Address{4}
	slip := pool.State{Key: domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindSlipstream}, Token0: a, Token1: b, TickSpacing: 10, Fee: 500}
	v4 := pool.State{Key: domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{4}}, Token0: a, Token1: b, TickSpacing: 10, Fee: 3000}
	base := state.New()
	base.Upsert(slip)
	base.Upsert(v4)
	table, err := graph.BuildTableFromCycles([]graph.Cycle{{Start: a, Pools: []domain.PoolKey{slip.Key, v4.Key}, Len: 2}})
	if err != nil {
		t.Fatal(err)
	}
	reader := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}}, deployments: domain.Deployments{V4PoolManager: manager, V4StateView: stateView}}
	view, _, err := prepareDependencies(reader, table, 1, false)(t.Context(), base.View(), []domain.PoolKey{slip.Key, v4.Key})
	if err != nil {
		t.Fatal(err)
	}
	if n := stateViewCalls.Load(); n != 0 {
		t.Fatalf("%d StateView reads for a hookless V4 fee", n)
	}
	if p, ok := view.Pool(v4.Key); !ok || p.Fee != 3000 {
		t.Fatalf("hookless V4 pool lost or changed: %v", ok)
	}
	if p, ok := view.Pool(slip.Key); !ok || p.Fee != 393 {
		t.Fatalf("Slipstream fee not reread: %v %d", ok, p.Fee)
	}
}
