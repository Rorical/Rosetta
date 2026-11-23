package arb

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Uses recorded OP pool identities and synthetic reserves. Measures shallow
// preparation only, without candidate selection, exact quotes, EVM or RPC.
func BenchmarkPackedPreparation(b *testing.B) {
	raw, err := os.ReadFile("../graph/testdata/op-mainnet-128-pools.json")
	if err != nil {
		b.Fatal(err)
	}
	var fixture struct {
		Pools []struct {
			Address, Token0, Token1, Hooks string
			PoolID                         string `json:"pool_id"`
			Kind                           domain.PoolKind
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		b.Fatal(err)
	}
	address := func(s string) domain.Address {
		a, err := domain.ParseAddress(s)
		if err != nil {
			b.Fatal(err)
		}
		return a
	}
	var states []pool.State
	var dirty []domain.PoolKey
	view := newFakeView()
	for _, row := range fixture.Pools {
		p := pool.State{Key: domain.PoolKey{Address: address(row.Address), Kind: row.Kind}, Token0: address(row.Token0), Token1: address(row.Token1), Reserve0: big.NewInt(1_000_000), Reserve1: big.NewInt(1_000_000)}
		if row.PoolID != "" {
			p.Key.PoolID, err = domain.ParseHash(row.PoolID)
			if err != nil {
				b.Fatal(err)
			}
		}
		if row.Hooks != "" {
			p.Key.Hooks = address(row.Hooks)
		}
		states = append(states, p)
		view.add(p)
		dirty = append(dirty, p.Key)
	}
	g, err := graph.Build(states)
	if err != nil {
		b.Fatal(err)
	}
	table, err := graph.BuildBalancedTableContext(b.Context(), g, 6, 500000)
	if err != nil && !errors.Is(err, graph.ErrTableFull) {
		b.Fatal(err)
	}
	table, err = graph.PackTable(b.Context(), table, states)
	if err != nil {
		b.Fatal(err)
	}
	engine, err := New(table, func(domain.Address) *big.Int { return big.NewInt(1) }, 8, identityQuoter())
	if err != nil {
		b.Fatal(err)
	}
	if err := engine.PreparePaths(b.Context(), states); err != nil {
		b.Fatal(err)
	}
	opts := RankedOptions{Bounds: func(domain.Address) (InputBounds, bool) {
		return InputBounds{Min: big.NewInt(1), Max: big.NewInt(1000)}, true
	}, SwapFee: func(pool.State, domain.Address) (uint32, error) { return 3000, nil }, Model: func(p pool.State) (pool.RankingModel, error) {
		return pool.RankingModel{Reserve0: p.Reserve0, Reserve1: p.Reserve1, ConstantProduct: true}, nil
	}}
	indices := table.Candidates(dirty, table.Len())
	for _, mode := range []string{"walk-paths", "dependency-union"} {
		b.Run(mode, func(b *testing.B) {
			snapshot := compiledSnapshots.Get().(*compiledSnapshot)
			defer snapshot.release()
			b.ReportAllocs()
			for b.Loop() {
				var ok bool
				var err error
				if mode == "walk-paths" {
					ok, err = snapshot.prepare(b.Context(), engine.compiled, indices, view, opts, 1)
				} else {
					ok, err = snapshot.prepareDirty(b.Context(), engine.compiled, dirty, view, opts, 1)
				}
				if err != nil || !ok {
					b.Fatal(ok, err)
				}
			}
		})
	}
}
