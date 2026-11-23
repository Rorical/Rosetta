package remote

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

type fixedCreationLogs struct{ log rpc.Log }

func (s fixedCreationLogs) GetLogs(context.Context, rpc.LogFilter) ([]rpc.Log, error) {
	return []rpc.Log{s.log}, nil
}
func abiAddress(a domain.Address) []byte { raw := make([]byte, 32); copy(raw[12:], a[:]); return raw }
func TestRemoteDiscoveryVerifiesFactoryAndReusesFixedHashReads(t *testing.T) {
	store, pools := poolFixture(t)
	anchor := store.Block()
	factory, newPool := addressAt(90), addressAt(30)
	var reserveReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch call.Method {
		case "eth_getCode":
			result = "0x00"
		case "eth_call":
			var input struct{ To, Data string }
			json.Unmarshal(call.Params[0], &input)
			var at struct {
				BlockHash        string
				RequireCanonical bool
			}
			json.Unmarshal(call.Params[1], &at)
			if at.BlockHash != anchor.Hash.Hex() || !at.RequireCanonical {
				t.Error("unbound discovery getter")
			}
			var raw []byte
			switch input.Data[:10] {
			case "0x" + hex.EncodeToString(selector("getPair(address,address)")):
				raw = abiAddress(newPool)
			case "0x" + hex.EncodeToString(selector("factory()")):
				raw = abiAddress(factory)
			case "0x" + hex.EncodeToString(selector("token0()")):
				raw = abiAddress(pools[0].Token0)
			case "0x" + hex.EncodeToString(selector("token1()")):
				raw = abiAddress(pools[0].Token1)
			case "0x0902f1ac":
				reserveReads.Add(1)
				raw = wordData(100000000, 100000000, 0)
			default:
				t.Error("unexpected getter", input.Data)
			}
			result = "0x" + hex.EncodeToString(raw)
		case "eth_getBlockByNumber":
			result = map[string]any{"number": "0xa", "hash": anchor.Hash.Hex(), "parentHash": anchor.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", anchor.Timestamp), "baseFeePerGas": "0x1", "gasLimit": "0x1000000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		default:
			t.Error(call.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer server.Close()
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
	data := append(abiAddress(newPool), wordData(1)...)
	logs := fixedCreationLogs{rpc.Log{Address: factory, Topics: []domain.Hash{topic0, token0, token1}, Data: data, BlockNumber: anchor.Number, BlockHash: anchor.Hash}}
	d := &poolDiscovery{config: discovery.Config{Deployments: &network.Network.DEX, LookbackBlocks: 100, ScanChunkBlocks: 100, MaxPools: 3, MaxFee: 10000, Sources: []discovery.Source{discovery.SourceUniswapV2}, Selection: &discovery.SelectionPolicy{Targets: []discovery.SelectionTarget{{Token: pools[0].Token0.Hex(), Amount: "100"}}, MaxTokens: 2, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 4}}, logs: logs}
	reader := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: anchor}, chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, deployments: network.Network.DEX}
	key := domain.PoolKey{Address: newPool, Kind: domain.PoolKindV2}
	job := discoveryTask{ctx: t.Context(), reader: reader, view: store.View(), topology: topology, activity: map[domain.PoolKey]discovery.Activity{key: {Swaps: 3}}}
	r := &runtime{policy: pipeline.SearchPolicy{Depth: 2, K: 8, MaxCycles: 100, WordRadius: 1, Funding: map[domain.Address]pipeline.SearchFunding{pools[0].Token0: {Seed: big.NewInt(100)}}}}
	answer := d.build(t.Context(), r, job)
	if answer.err != nil || answer.added == nil || answer.topology == nil {
		t.Fatal(answer.err, answer)
	}
	if _, ok := answer.added.Pool(key); !ok {
		t.Fatal("eligible new market was not installed")
	}
	if reserveReads.Load() != 1 {
		t.Fatal("verification, shallow screening and seeding repeated reserve reads", reserveReads.Load())
	}
}
func TestRemoteDiscoveryRebasesReceiptTailBeforeGraphPublication(t *testing.T) {
	t.Run("canonical", func(t *testing.T) { remoteDiscoveryRebase(t, false) })
	t.Run("Arbitrum Fast suffix", func(t *testing.T) { remoteDiscoveryRebase(t, true) })
}

func remoteDiscoveryRebase(t *testing.T, withFast bool) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, pools := poolFixture(t)
	anchor := store.Block()
	newState := pools[0].Copy()
	newState.Key.Address = addressAt(30)
	newState.Reserve1 = big.NewInt(1000000)
	added := poolstate.New()
	if err := added.BeginBlock(anchor); err != nil {
		t.Fatal(err)
	}
	if err := added.Upsert(newState); err != nil {
		t.Fatal(err)
	}
	added.Commit()
	options := pipeline.TopologyOptions{Depth: 2, K: 8, MaxCycles: 100, Notional: func(domain.Address) *big.Int { return big.NewInt(100) }}
	old, _ := pipeline.BuildTopology(ctx, pools, options)
	nextTopology, err := pipeline.ExtendTopology(ctx, append(pools, newState), old, []domain.PoolKey{newState.Key}, options)
	if err != nil {
		t.Fatal(err)
	}
	next := anchor
	next.Number++
	next.ParentHash = anchor.Hash
	next.Hash = domain.Hash{11}
	log := syncEvent(newState.Key, 3000000)
	tx := ingest.Transaction{Hash: domain.Hash{33}, Index: 0, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}}
	branch, err := ingest.NewTransactionBranch(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.Begin(next); err != nil {
		t.Fatal(err)
	}
	if _, err := branch.Apply(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := branch.Commit(next, []domain.Hash{tx.Hash}); err != nil {
		t.Fatal(err)
	}
	worker, err := arb.NewWorker(ctx, old.Engine, arb.RankedOptions{SwapFee: dex.SwapFee, Bounds: func(domain.Address) (arb.InputBounds, bool) {
		return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(1000)}, true
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); <-worker.Done() }()
	fast, err := ingest.NewFastBranch(branch.Base(), ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := &runtime{cfg: config.Config{Network: config.Network{Family: domain.FamilyNitro, ChainID: 42161}}, store: branch.Base(), block: rpc.ExecutionBlock{Ref: next}, fast: fast, worker: worker, topology: old, search: pipeline.NewSearchScheduler(worker, 1, nil)}
	if withFast {
		r.cfg.FastURL = "enabled"
		for number := uint64(12); number <= 13; number++ {
			if _, err := fast.Apply(ctx, feed.FastMessage{BlockNumber: number, TxHash: domain.Hash{byte(number)}, RawTx: []byte{byte(number)}, Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{syncEvent(newState.Key, int64(number)*300000)}}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	d := &poolDiscovery{tail: []canonicalUpdate{{block: rpc.ExecutionBlock{Ref: next, Transactions: []domain.Hash{tx.Hash}}, transactions: []ingest.Transaction{tx}}}}
	answer := discoveryAnswer{anchor: anchor, added: added, topology: nextTopology, graph: 1}
	if err := d.consume(ctx, r, answer); err != nil {
		t.Fatal(err)
	}
	if withFast {
		view, version, _, ok := fast.View()
		loaded, found := view.Pool(newState.Key)
		if !ok || !found || version.BlockNumber != 13 || loaded.Reserve1.Int64() != 3900000 {
			t.Fatal("catalog update discarded the Arbitrum Fast suffix")
		}
	}
	loaded, ok := r.store.Pool(newState.Key)
	if !ok || loaded.Reserve1.Int64() != 3000000 || r.search.GraphVersion != 2 || r.store.Block() != next {
		t.Fatal("stale discovery state crossed graph publication", loaded, r.store.Block())
	}
}
