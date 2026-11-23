package remote

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type creationLogs []rpc.Log

func (s creationLogs) GetLogs(context.Context, rpc.LogFilter) ([]rpc.Log, error) { return s, nil }

// A creation log marks the next round urgent. Rounds still keep a gap, so a
// steady stream of creations cannot run them back to back.
func TestUrgentDiscoveryRoundsKeepAGap(t *testing.T) {
	store, _ := poolFixture(t)
	d := &poolDiscovery{config: discovery.Config{RefreshBlocks: 900}, jobs: make(chan discoveryTask, 1), ctx: t.Context(), last: 1000, urgent: true}
	r := &runtime{cfg: config.Config{Network: config.Network{Family: domain.FamilyOPStack, BlockTimeMS: 2000}}, store: store, search: &pipeline.SearchScheduler{}}
	gap := r.cfg.Network.Blocks(discoveryUrgentGap)
	at := func(number uint64) bool {
		r.block = rpc.ExecutionBlock{Ref: domain.BlockRef{Number: number}}
		d.schedule(r)
		select {
		case <-d.jobs:
			d.busy = false
			return true
		default:
			return false
		}
	}
	if at(1000 + gap - 1) {
		t.Fatal("urgent round ran inside the gap")
	}
	if !at(1000 + gap) {
		t.Fatal("urgent round did not run after the gap")
	}
	if d.urgent || at(1000+gap+1) {
		t.Fatal("a round left the urgent mark set")
	}
	if !at(1000 + gap + 900) {
		t.Fatal("periodic round did not run")
	}
}

// A round used to re-verify every unadopted candidate and retry every rejected
// one. Identities are verified once, rejections wait an activity window, and
// both survive a restart through the index.
func TestDiscoveryVerifiesOnceAndRemembersRejections(t *testing.T) {
	store, pools := poolFixture(t)
	anchor := store.Block()
	factory, registered, unregistered := addressAt(90), addressAt(30), addressAt(31)
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		json.NewDecoder(request.Body).Decode(&call)
		var result any
		name := call.Method
		switch call.Method {
		case "eth_getCode":
			result = "0x00"
		case "eth_call":
			var input struct{ To, Data string }
			json.Unmarshal(call.Params[0], &input)
			var raw []byte
			switch input.Data[:10] {
			case "0x" + hex.EncodeToString(selector("getPair(address,address)")):
				name, raw = "getPair", abiAddress(registered)
			case "0x" + hex.EncodeToString(selector("factory()")):
				name, raw = "factory", abiAddress(factory)
			case "0x" + hex.EncodeToString(selector("token0()")):
				name, raw = "token0", abiAddress(pools[0].Token0)
			case "0x" + hex.EncodeToString(selector("token1()")):
				name, raw = "token1", abiAddress(pools[0].Token1)
			case "0x0902f1ac":
				name, raw = "reserves", wordData(100000000, 100000000, 0)
			default:
				t.Error("unexpected getter", input.Data)
			}
			result = "0x" + hex.EncodeToString(raw)
		case "eth_getBlockByNumber":
			result = map[string]any{"number": "0xa", "hash": anchor.Hash.Hex(), "parentHash": anchor.ParentHash.Hex(), "timestamp": "0x1", "baseFeePerGas": "0x1", "gasLimit": "0x1000000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		default:
			t.Error(call.Method)
		}
		mu.Lock()
		calls[name]++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer server.Close()
	count := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := calls
		calls = map[string]int{}
		return out
	}
	network, _ := (config.Config{}).ResolveNetwork()
	network.Network.DEX.Factories["uniswap_v2"] = factory
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, K: 8, MaxCycles: 100, Notional: func(domain.Address) *big.Int { return big.NewInt(100) }})
	if err != nil {
		t.Fatal(err)
	}
	topic0 := digest([]byte("PairCreated(address,address,address,uint256)"))
	var token0, token1 domain.Hash
	copy(token0[12:], pools[0].Token0[:])
	copy(token1[12:], pools[0].Token1[:])
	created := func(pool domain.Address) rpc.Log {
		return rpc.Log{Address: factory, Topics: []domain.Hash{topic0, token0, token1}, Data: append(abiAddress(pool), wordData(1)...), BlockNumber: anchor.Number, BlockHash: anchor.Hash}
	}
	cfg := discovery.Config{Deployments: &network.Network.DEX, LookbackBlocks: 100, ScanChunkBlocks: 100, MaxPools: 3, MaxFee: 10000, Sources: []discovery.Source{discovery.SourceUniswapV2}, Selection: &discovery.SelectionPolicy{Targets: []discovery.SelectionTarget{{Token: pools[0].Token0.Hex(), Amount: "100"}}, MaxTokens: 2, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 4}}
	path := filepath.Join(t.TempDir(), "index.json")
	logs := creationLogs{created(registered), created(unregistered)}
	open := func() *poolDiscovery {
		index := &discovery.CreationIndex{Source: logs, Path: path, ChainID: 42161}
		if _, err := index.Load(); err != nil {
			t.Fatal(err)
		}
		d := &poolDiscovery{config: cfg, logs: index, index: index}
		d.restoreVerdicts()
		return d
	}
	activity := map[domain.PoolKey]discovery.Activity{{Address: registered, Kind: domain.PoolKindV2}: {Swaps: 3}, {Address: unregistered, Kind: domain.PoolKindV2}: {Swaps: 3}}
	r := &runtime{policy: pipeline.SearchPolicy{Depth: 2, K: 8, MaxCycles: 100, WordRadius: 1, Funding: map[domain.Address]pipeline.SearchFunding{pools[0].Token0: {Seed: big.NewInt(100)}}}}
	generation := uint64(0)
	round := func(d *poolDiscovery) discoveryAnswer {
		t.Helper()
		// A fresh snapshot per round: its read cache must not hide repeats.
		reader := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: anchor}, chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, deployments: network.Network.DEX}
		answer := d.build(t.Context(), r, discoveryTask{ctx: t.Context(), reader: reader, view: store.View(), topology: topology, activity: activity, generation: generation})
		if answer.err != nil {
			t.Fatal(answer.err)
		}
		return answer
	}

	d := open()
	first := round(d)
	if _, ok := first.added.Pool(domain.PoolKey{Address: registered, Kind: domain.PoolKindV2}); !ok {
		t.Fatal("registered market was not installed")
	}
	if got := count(); got["getPair"] == 0 || got["factory"] != 1 {
		t.Fatal("first round did not verify both candidates", got)
	}
	if !d.cooling(domain.PoolKey{Address: unregistered, Kind: domain.PoolKindV2}, anchor.Number) {
		t.Fatal("unregistered candidate was not remembered")
	}

	// The answer went stale, so the registered pool is still unknown. A new
	// process reloads it from the index without verifying either pool again.
	again := round(open())
	if _, ok := again.added.Pool(domain.PoolKey{Address: registered, Kind: domain.PoolKindV2}); !ok {
		t.Fatal("verified market was dropped on the second round")
	}
	if got := count(); got["getPair"] != 0 || got["factory"] != 0 || got["token0"] != 0 || got["reserves"] == 0 {
		t.Fatal("second round repeated identity reads or skipped the state load", got)
	}

	// A reorg starts a new generation: verdicts from the abandoned branch go
	// with the index, and both pools are judged again.
	d = open()
	generation++
	round(d)
	if got := count(); got["factory"] != 1 || got["getPair"] == 0 {
		t.Fatal("verdicts survived a reorg reset", got)
	}

	// Any configuration change, here the fee cap, discards saved verdicts.
	cfg.MaxFee = 5000
	reconfigured := open()
	if len(reconfigured.verdicts) != 0 || reconfigured.cooling(domain.PoolKey{Address: unregistered, Kind: domain.PoolKindV2}, anchor.Number) {
		t.Fatal("verdicts outlived a configuration change", reconfigured.verdicts)
	}
}

// Seed rows are admitted by configuration alone; a cached verdict must not
// keep admitting a seed after it is removed.
func TestDiscoverySeedsAreNotCached(t *testing.T) {
	seed := discovery.Spec{Address: addressAt(40), Kind: domain.PoolKindV2, Token0: addressAt(1), Token1: addressAt(2), Fee: 3000}
	d := &poolDiscovery{config: discovery.Config{Seeds: []discovery.Spec{seed}}}
	row := discovery.Creation{Spec: seed}
	if _, active, cached, err := d.identity(t.Context(), nil, row); err != nil || !active || cached {
		t.Fatal("configured seed", active, cached, err)
	}
	d.config.Seeds = nil
	if _, _, _, err := d.identity(t.Context(), nil, row); err == nil {
		t.Fatal("removed seed still admitted")
	}
}

// A token's code is read once per process and judged against the configured
// templates; tokens without a match are cached too.
func TestDiscoveryDeniesTokenTemplatesReadingCodeOnce(t *testing.T) {
	trap, plain := addressAt(50), addressAt(51)
	var mu sync.Mutex
	reads := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var call struct {
			ID     uint64
			Params []json.RawMessage
		}
		json.NewDecoder(request.Body).Decode(&call)
		var address string
		json.Unmarshal(call.Params[0], &address)
		code := "0x6000"
		if address == trap.Hex() {
			code = "0x60016300000000630174639d"
		}
		mu.Lock()
		reads[address]++
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": code})
	}))
	defer server.Close()
	config, err := discovery.LoadConfig([]byte(`{"deny_token_code_selectors":["0x0174639d"]}`))
	if err != nil {
		t.Fatal(err)
	}
	d := &poolDiscovery{config: config}
	reader := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{1}}}}
	for range 2 {
		if denied, err := d.deniedToken(t.Context(), reader, trap); err != nil || !denied {
			t.Fatal("template token admitted", err)
		}
		if denied, err := d.deniedToken(t.Context(), reader, plain); err != nil || denied {
			t.Fatal("plain token denied", err)
		}
	}
	if reads[trap.Hex()] != 1 || reads[plain.Hex()] != 1 {
		t.Fatal("token code read more than once", reads)
	}
}

// A verified pool that no funded cycle selected waits one refresh interval, not
// a full activity window, so multi-hop cycles whose pools verify in different
// rounds can still assemble. Both keep the verified identity.
func TestDeferUnselectedUsesShortCooldownAndKeepsIdentity(t *testing.T) {
	cfg := discovery.Config{RefreshBlocks: 2400, Selection: &discovery.SelectionPolicy{ActivityWindow: 144000}}
	d := &poolDiscovery{config: cfg}
	key := domain.PoolKey{Address: domain.Address{7}, Kind: domain.PoolKindV2}
	spec := bootstrap.Spec{Address: domain.Address{7}, Kind: domain.PoolKindV2, Token0: domain.Address{1}, Token1: domain.Address{2}, Fee: 3000}
	d.remember(key, discovery.Verdict{Key: key, Spec: &spec})

	d.deferUnselected(key, 1000)
	if got := d.verdicts[key].Until; got != 1000+2400 {
		t.Fatalf("defer cooldown = %d, want %d", got, 1000+2400)
	}
	d.reject(key, 1000)
	if got := d.verdicts[key].Until; got != 1000+144000 {
		t.Fatalf("reject cooldown = %d, want %d", got, 1000+144000)
	}
	if d.verdicts[key].Spec == nil {
		t.Fatal("verified identity dropped")
	}
	if !d.cooling(key, 1000+2399) || d.cooling(key, 1000+144000) {
		t.Fatal("cooling window wrong")
	}
}
