package arb

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

// frameFixture replays the 44-pool OP prefix at the live search shape: a
// packed depth-6 table, K=32 and nine funding assets. The changed view moves
// every reserve-based pool by 0.1%, as a flashblock touching those pools would.
type frameFixture struct {
	engine          *Engine
	base, changed   View
	all, reserveSet []domain.PoolKey
	opts            RankedOptions
}

func newFrameFixture(tb testing.TB) frameFixture {
	tb.Helper()
	raw, err := os.ReadFile("testdata/op_rank_depth_158986419.json")
	if err != nil {
		tb.Fatal(err)
	}
	var recorded struct {
		Pools []pool.State
		Deep  []state.DeepDelta
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		tb.Fatal(err)
	}
	load := func(pools []pool.State) *state.Store {
		store := state.New()
		for _, p := range pools {
			if err := store.Upsert(p); err != nil {
				tb.Fatal(err)
			}
		}
		for _, d := range recorded.Deep {
			if err := store.SetDeep(d.Key, d.State); err != nil {
				tb.Fatal(err)
			}
		}
		return store
	}
	var reserveSet []domain.PoolKey
	changed := make([]pool.State, len(recorded.Pools))
	for i, p := range recorded.Pools {
		changed[i] = p
		if p.Reserve0 != nil && p.Reserve0.Sign() > 0 {
			reserve := new(big.Int).Mul(p.Reserve0, big.NewInt(1001))
			changed[i].Reserve0 = reserve.Quo(reserve, big.NewInt(1000))
			reserveSet = append(reserveSet, p.Key)
		}
	}
	g, err := graph.Build(recorded.Pools)
	if err != nil {
		tb.Fatal(err)
	}
	table, err := graph.BuildTable(g, 6, 500000)
	if err != nil {
		tb.Fatal(err)
	}
	table, err = graph.PackTable(tb.Context(), table, recorded.Pools)
	if err != nil {
		tb.Fatal(err)
	}
	quoter, err := simulate.NewQuoter(dex.NewAdapter)
	if err != nil {
		tb.Fatal(err)
	}
	funding := frameFunding(tb)
	engine, err := New(table, func(token domain.Address) *big.Int { return funding[token].seed }, 32, quoter)
	if err != nil {
		tb.Fatal(err)
	}
	if err := engine.PreparePaths(tb.Context(), recorded.Pools); err != nil {
		tb.Fatal(err)
	}
	base := load(recorded.Pools).View()
	opts := RankedOptions{StreamInitial: true, Diverse: true, SwapFee: dex.SwapFee, Model: dex.RankingModel,
		Bounds: func(token domain.Address) (InputBounds, bool) {
			entry, ok := funding[token]
			return entry.bounds, ok
		}}
	return frameFixture{engine: engine, base: base, changed: load(changed).View(), all: base.PoolKeys(), reserveSet: reserveSet, opts: opts}
}

type frameFundingEntry struct {
	bounds InputBounds
	seed   *big.Int
}

// Nine OP funding assets with dollar-comparable bounds: about $0.01 to $1000
// and a $100 seed, with a floor near the cheapest executor transaction.
func frameFunding(tb testing.TB) map[domain.Address]frameFundingEntry {
	entry := func(minimum, seed, maximum, floor string) frameFundingEntry {
		value := func(text string) *big.Int {
			out, _ := new(big.Int).SetString(text, 10)
			return out
		}
		return frameFundingEntry{bounds: InputBounds{Min: value(minimum), Max: value(maximum), Floor: value(floor)}, seed: value(seed)}
	}
	dollar6 := entry("10000", "100000000", "1000000000", "20")
	ether := entry("1000000000000", "30000000000000000", "500000000000000000", "10000000000")
	dollar18 := entry("10000000000000000", "100000000000000000000", "1000000000000000000000", "20000000000000")
	funding := make(map[domain.Address]frameFundingEntry)
	for token, value := range map[string]frameFundingEntry{
		"0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85": dollar6,  // USDC
		"0x7F5c764cBc14f9669B88837ca1490cCa17c31607": dollar6,  // USDC.e
		"0x94b008aA00579c1307B0EF2c499aD98a8ce58e58": dollar6,  // USDT
		"0x01bFF41798a0BcF287b996046Ca68b395DbC1071": dollar6,  // USDT0
		"0x4200000000000000000000000000000000000006": ether,    // WETH
		"0x1F32b1c2345538c0c6f582fCB022739c4A194Ebb": ether,    // wstETH
		"0x4200000000000000000000000000000000000042": dollar18, // OP
		"0xDA10009cBd5D07dd0CeCc66161FC93D7c9000da1": dollar18, // DAI
		"0x68f180fcCe6836688e9084f035309E29Bf0A2095": entry("10", "100000", "1000000", "1"),
	} {
		address, err := domain.ParseAddress(token)
		if err != nil {
			tb.Fatal(err)
		}
		funding[address] = value
	}
	return funding
}
