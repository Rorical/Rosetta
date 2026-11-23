package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestRemoteDiscoveryVisitsLaterIndexedMarketsAcrossRounds(t *testing.T) {
	store, pools := poolFixture(t)
	network, err := (config.Config{}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	index := &discovery.CreationIndex{}
	activity := map[domain.PoolKey]discovery.Activity{}
	for i := 0; i < 6; i++ {
		row := discovery.Creation{Source: discovery.SourceUniswapV2, Spec: discovery.Spec{Address: addressAt(byte(100 + i)), Kind: domain.PoolKindV2, Token0: pools[0].Token0, Token1: pools[0].Token1, Fee: 3000}}
		index.Remember([]discovery.Creation{row})
		activity[row.Spec.PoolKey()] = discovery.Activity{Swaps: uint64(100 - i)}
	}
	d := &poolDiscovery{index: index, logs: fixedCreationLogs{}, config: discovery.Config{Deployments: &network.Network.DEX, LookbackBlocks: 10, ScanChunkBlocks: 10, MaxPools: 32, MaxFee: 10000, Sources: []discovery.Source{discovery.SourceUniswapV2}, Selection: &discovery.SelectionPolicy{Targets: []discovery.SelectionTarget{{Token: pools[0].Token0.Hex(), Amount: "1"}}, MaxTokens: 32, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 2}}}
	job := discoveryTask{ctx: t.Context(), reader: &snapshot{block: rpc.ExecutionBlock{Ref: store.Block()}}, view: store.View(), activity: activity}
	seen := map[domain.PoolKey]bool{}
	for range 3 {
		rows, err := d.candidateCreations(t.Context(), job)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if seen[row.Spec.PoolKey()] {
				t.Fatal("indexed shortlist repeated while later active markets were unchecked")
			}
			seen[row.Spec.PoolKey()] = true
		}
	}
	if len(seen) != 6 {
		t.Fatal("indexed candidate coverage did not grow")
	}
}

func TestDiscoveryRetainsInitializationTailBeyond128Blocks(t *testing.T) {
	store, _ := poolFixture(t)
	r := &runtime{store: store}
	d := &poolDiscovery{config: discovery.Config{LookbackBlocks: 1000}}
	ref := store.Block()
	for range 200 {
		ref.ParentHash = ref.Hash
		ref.Number++
		ref.Hash = domain.Hash{byte(ref.Number), byte(ref.Number >> 8)}
		d.observe(canonicalUpdate{block: rpc.ExecutionBlock{Ref: ref}}, r)
	}
	if len(d.tail) != 200 || d.tail[0].block.Ref.Number != 11 {
		t.Fatal("slow pool initialization lost its canonical replay ancestry", len(d.tail))
	}
}

func TestDiscoveryReplayTailStillHonorsByteBound(t *testing.T) {
	d := &poolDiscovery{}
	data := make([]byte, 1<<20)
	for range 200 {
		d.appendTail(canonicalUpdate{transactions: []ingest.Transaction{{Hash: domain.Hash{1}, Successful: true, Logs: []ingest.Log{{Data: data}}}}})
	}
	if len(d.tail) == 0 || len(d.tail) >= 200 || d.bytes > discoveryTailBytes {
		t.Fatal("larger block window bypassed the receipt byte bound", len(d.tail), d.bytes)
	}
}

// 1,024 retained blocks were 105 seconds on Robinhood, shorter than one
// discovery round; the replay window is ten minutes of the chain's blocks.
func TestDiscoveryReplayTailSpansMinutesOfFastBlocks(t *testing.T) {
	blocks := int(config.Network{Family: domain.FamilyNitro, BlockTimeMS: 100}.Blocks(discoveryTailSpan))
	d := &poolDiscovery{tailBlocks: blocks}
	for i := range 3000 {
		d.appendTail(canonicalUpdate{block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: uint64(i)}}})
	}
	if blocks != 6000 || len(d.tail) != 3000 {
		t.Fatalf("window %d blocks kept %d of 3000", blocks, len(d.tail))
	}
}

// With a state endpoint the round reads its anchor there, pinned to the same
// block, instead of on a primary that keeps 128 blocks of state.
func TestDiscoveryReaderUsesTheStateEndpoint(t *testing.T) {
	store, _ := poolFixture(t)
	r := &runtime{cfg: config.Config{Network: config.Network{Family: domain.FamilyNitro, ChainID: 4663}}, client: rpc.NewClient("http://127.0.0.1:1", rpc.Options{}), store: store, block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 7, Hash: domain.Hash{7}}}}
	archive := rpc.NewClient("http://127.0.0.1:2", rpc.Options{})
	d := &poolDiscovery{state: archive}
	reader := d.reader(r)
	if reader.client != archive || reader.block.Ref != r.block.Ref {
		t.Fatal("anchor reads did not use the state endpoint at the anchor")
	}
	d.state = nil
	if reader = d.reader(r); reader.client != r.client {
		t.Fatal("without a state endpoint the canonical reader is used")
	}
	reader.Close()
}

// The state endpoint can trail the primary by a few blocks and answer
// "header not found" for a just-sealed anchor. A round waits
// for the endpoint to serve its anchor, and gives up after its budget.
func TestDiscoveryWaitsForTheStateEndpointAnchor(t *testing.T) {
	anchor := domain.BlockRef{Number: 0x10, Hash: domain.Hash{0x10}}
	var asked atomic.Int32
	serveAfter := int32(2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&call)
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": nil}
		if asked.Add(1) > serveAfter {
			response["result"] = map[string]any{"number": "0x10", "hash": anchor.Hash.Hex(), "parentHash": domain.Hash{}.Hex(), "timestamp": "0x1", "baseFeePerGas": "0x1", "gasLimit": "0x1c9c380", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		}
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	d := &poolDiscovery{state: rpc.NewClient(server.URL, rpc.Options{})}
	if err := d.awaitStateAnchor(t.Context(), anchor, time.Second, time.Millisecond); err != nil || asked.Load() != 3 {
		t.Fatalf("waited %d queries: %v", asked.Load(), err)
	}
	asked.Store(0)
	serveAfter = 1 << 30
	if err := d.awaitStateAnchor(t.Context(), anchor, 20*time.Millisecond, time.Millisecond); err == nil {
		t.Fatal("a missing anchor was accepted")
	}
}
