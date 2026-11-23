package arb

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

func recordedSearchFixture(tb testing.TB) (*Engine, View, []domain.PoolKey, RankedOptions) {
	tb.Helper()
	raw, err := os.ReadFile("testdata/op_prefix_158942837.json")
	if err != nil {
		tb.Fatal(err)
	}
	var fixture struct {
		Pools   []pool.State      `json:"pools"`
		Deep    []state.DeepDelta `json:"deep"`
		Funding []struct {
			Token          domain.Address
			Min, Max, Seed *big.Int
		} `json:"funding"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		tb.Fatal(err)
	}
	store := state.New()
	for _, p := range fixture.Pools {
		if err := store.Upsert(p); err != nil {
			tb.Fatal(err)
		}
	}
	for _, d := range fixture.Deep {
		if err := store.SetDeep(d.Key, d.State); err != nil {
			tb.Fatal(err)
		}
	}
	funding := make(map[domain.Address]InputBounds)
	seeds := make(map[domain.Address]*big.Int)
	for _, f := range fixture.Funding {
		funding[f.Token] = InputBounds{Min: f.Min, Max: f.Max}
		seeds[f.Token] = f.Seed
	}
	g, err := graph.Build(fixture.Pools)
	if err != nil {
		tb.Fatal(err)
	}
	table, err := graph.BuildTable(g, 6, 500000)
	if err != nil {
		tb.Fatal(err)
	}
	table, err = graph.PackTable(tb.Context(), table, fixture.Pools)
	if err != nil {
		tb.Fatal(err)
	}
	quoter, err := simulate.NewQuoter(dex.NewAdapter)
	if err != nil {
		tb.Fatal(err)
	}
	engine, err := New(table, func(token domain.Address) *big.Int { return seeds[token] }, 32, quoter)
	if err != nil {
		tb.Fatal(err)
	}
	if err := engine.PreparePaths(tb.Context(), fixture.Pools); err != nil {
		tb.Fatal(err)
	}
	view := store.View()
	opts := RankedOptions{Bounds: func(token domain.Address) (InputBounds, bool) { bounds, ok := funding[token]; return bounds, ok }, SwapFee: dex.SwapFee, Model: dex.RankingModel}
	return engine, view, view.PoolKeys(), opts
}

// Fixed recorded pool inputs, complete ranking and exact sizing, plus streaming
// and ownership handoff. A new block each iteration prevents exact-cache hits.
func BenchmarkRecordedSearch(b *testing.B) {
	engine, view, dirty, opts := recordedSearchFixture(b)
	ctx, cancel := context.WithCancel(b.Context())
	w, err := NewWorker(ctx, engine, opts)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { cancel(); <-w.Done() }()
	job := SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}
	selected := 0
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		job.Version.BlockNumber++
		if err := w.Submit(job); err != nil {
			b.Fatal(err)
		}
		for {
			e := <-w.Results()
			if e.Err != nil || !w.Rebase(&e) || !w.Accept(e) {
				b.Fatal("recorded search", e.Err)
			}
			releaseSearch(e.Job)
			if !e.Partial {
				selected = e.Result.Selected
				break
			}
		}
	}
	b.ReportMetric(float64(selected), "selected/op")
}

// A frame dirties a few pools, not all of them. The four pools on the most
// recorded paths stand in for a busy frame on hub pools; run with -cpu to see
// what more cores buy each lane.
func BenchmarkRecordedFrame(b *testing.B) {
	engine, view, all, opts := recordedSearchFixture(b)
	paths := make(map[domain.PoolKey]int, len(all))
	for i := range engine.table.Len() {
		for _, key := range engine.table.Cycle(i).Pools {
			paths[key]++
		}
	}
	dirty := slices.Clone(all)
	slices.SortStableFunc(dirty, func(a, c domain.PoolKey) int { return paths[c] - paths[a] })
	dirty = dirty[:4]
	b.Run("fast_rank", func(b *testing.B) {
		for b.Loop() {
			if _, err := engine.Rank(b.Context(), view, dirty, opts); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("fast_lane", func(b *testing.B) {
		for b.Loop() {
			if _, err := engine.EvaluateRanked(b.Context(), view, dirty, opts); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("worker", func(b *testing.B) {
		ctx, cancel := context.WithCancel(b.Context())
		w, err := NewWorker(ctx, engine, opts)
		if err != nil {
			b.Fatal(err)
		}
		defer func() { cancel(); <-w.Done() }()
		job := SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}
		for b.Loop() {
			job.Version.BlockNumber++
			if err := w.Submit(job); err != nil {
				b.Fatal(err)
			}
			for {
				e := <-w.Results()
				if e.Err != nil || !w.Rebase(&e) || !w.Accept(e) {
					b.Fatal("recorded frame", e.Err)
				}
				releaseSearch(e.Job)
				if !e.Partial {
					break
				}
			}
		}
	})
}
