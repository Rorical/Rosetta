package remote

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func TestExactDependenciesReuseFreshLogMaintainedWindow(t *testing.T) {
	store := poolstate.New()
	key := domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindV3}
	p := pool.State{Key: key, Token0: domain.Address{2}, Token1: domain.Address{3}, Fee: 500, TickSpacing: 10, Tick: -1, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1000)}
	if err := store.Upsert(p); err != nil {
		t.Fatal(err)
	}
	deep := pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}, WindowMin: -5120, WindowMax: 2559}
	if err := store.SetDeep(key, deep); err != nil {
		t.Fatal(err)
	}
	// A nil RPC client deliberately makes any unexpected network read fail.
	reader := &snapshot{}
	view, issues, err := prepareDependencies(reader, nil, 1, true)(context.Background(), store.View(), []domain.PoolKey{key})
	if err != nil || len(issues) != 0 {
		t.Fatal(issues, err)
	}
	if got, ok := view.Deep(key); !ok || got.WindowMin != deep.WindowMin {
		t.Fatal("fresh deep state lost")
	}
	p.Tick = -2561
	if coversDeepWindow(p, deep, 1) {
		t.Fatal("negative tick moved into an uncovered word")
	}
	p.Tick = math.MinTick
	deep.WindowMin = math.MinTick
	deep.WindowMax = math.MinTick + 10000
	if !coversDeepWindow(p, deep, 1) {
		t.Fatal("protocol minimum was not clamped")
	}
}

func TestRetainDependenciesOnlyFromCurrentStandardView(t *testing.T) {
	key := domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindV3}
	p := pool.State{Key: key, TickSpacing: 10, Tick: 0}
	block := domain.BlockRef{Number: 10, Hash: domain.Hash{10}}
	version := domain.StateVersion{Source: domain.SourceStandard, Anchor: block, BlockNumber: 10, Epoch: 1}
	for _, test := range []struct {
		name   string
		source domain.StateSource
		epoch  uint64
		loaded domain.BlockRef
		want   bool
	}{
		{"current", domain.SourceStandard, 1, block, true},
		{"old generation", domain.SourceStandard, 0, block, false},
		{"tentative", domain.SourceFast, 1, block, false},
		{"other block", domain.SourceStandard, 1, domain.BlockRef{Number: 9}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := poolstate.New()
			if err := store.BeginBlock(block); err != nil {
				t.Fatal(err)
			}
			if err := store.Upsert(p); err != nil {
				t.Fatal(err)
			}
			r := &runtime{store: store, version: version}
			deep := pool.DeepState{WindowMin: -2560, WindowMax: 5119, LoadedBlock: test.loaded}
			// Exact dependencies wrap the preliminary fee/dependency view.
			inner := &dependencyView{View: store.View()}
			view := &dependencyView{View: inner, deep: map[domain.PoolKey]pool.DeepState{key: deep}}
			eventVersion := version
			eventVersion.Source = test.source
			eventVersion.Epoch = test.epoch
			event := arb.SearchEvent{Job: arb.SearchJob{View: view, Version: eventVersion}}
			if err := r.retainDependencies(event); err != nil {
				t.Fatal(err)
			}
			if _, ok := store.View().Deep(key); ok != test.want {
				t.Fatalf("retained=%v want=%v", ok, test.want)
			}
			if !test.want {
				return
			}
			next := block
			next.Number++
			next.Hash = domain.Hash{11}
			next.ParentHash = block.Hash
			if err := store.BeginBlock(next); err != nil {
				t.Fatal(err)
			}
			// The next block must reuse the refreshed window without network access.
			_, issues, err := prepareDependencies(&snapshot{}, nil, 1, true)(t.Context(), store.View(), []domain.PoolKey{key})
			if err != nil || len(issues) != 0 {
				t.Fatal(issues, err)
			}
		})
	}
}
