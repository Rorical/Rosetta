package arb

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestRecordedOPPrefixKeepsPositiveQuotesThroughCompiledCache(t *testing.T) {
	raw, err := os.ReadFile("testdata/op_prefix_158942837.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Pools   []pool.State      `json:"pools"`
		Deep    []state.DeepDelta `json:"deep"`
		Funding []struct {
			Token          domain.Address
			Min, Max, Seed *big.Int
		} `json:"funding"`
		Expected []struct {
			Cycle      graph.Cycle
			Input, Net *big.Int
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	store := state.New()
	for _, p := range fixture.Pools {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range fixture.Deep {
		if err := store.SetDeep(d.Key, d.State); err != nil {
			t.Fatal(err)
		}
	}
	funding := make(map[domain.Address]InputBounds)
	seeds := make(map[domain.Address]*big.Int)
	for _, f := range fixture.Funding {
		funding[f.Token] = InputBounds{Min: f.Min, Max: f.Max}
		seeds[f.Token] = f.Seed
	}
	opts := RankedOptions{Bounds: func(token domain.Address) (InputBounds, bool) { bounds, ok := funding[token]; return bounds, ok }, SwapFee: dex.SwapFee}
	g, err := graph.Build(fixture.Pools)
	if err != nil {
		t.Fatal(err)
	}
	table, err := graph.BuildTable(g, 6, 500000)
	if err != nil {
		t.Fatal(err)
	}
	quoter, err := simulate.NewQuoter(dex.NewAdapter)
	if err != nil {
		t.Fatal(err)
	}
	notional := func(token domain.Address) *big.Int { return seeds[token] }
	generic, err := New(table, notional, 32, quoter)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := New(table, notional, 32, quoter)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.PreparePaths(t.Context(), fixture.Pools); err != nil {
		t.Fatal(err)
	}
	view := store.View()
	dirty := view.PoolKeys()
	expected, err := generic.EvaluateRanked(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	var cache rankCache
	cache.bind(compiled.compiled, 1)
	for range 3 {
		selection, err := compiled.rank(t.Context(), view, dirty, opts, &cache)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := compiled.Optimize(t.Context(), view, selection)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("compiled/cache output differs from generic quote on recorded prefix")
		}
		for _, vector := range fixture.Expected {
			found := false
			for _, op := range actual.Opportunities {
				if reflect.DeepEqual(op.Cycle, vector.Cycle) && op.Input.Cmp(vector.Input) == 0 && op.Net.Cmp(vector.Net) == 0 {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("recorded positive route lost: %s input=%s gross=%s", vector.Cycle.Start, vector.Input, vector.Net)
			}
		}
	}
	// Warm coarse estimates must not reuse an earlier successful tick quote.
	missing := missingRecordedTicks{View: view, key: fixture.Expected[0].Cycle.Pools[0]}
	selection, err := compiled.rank(t.Context(), missing, dirty, opts, &cache)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := compiled.Optimize(t.Context(), missing, selection)
	if err != nil {
		t.Fatal(err)
	}
	expected, err = generic.EvaluateRanked(t.Context(), missing, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("coarse cache reused an old tick quote")
	}
	foundMissing := false
	for _, issue := range actual.Issues {
		if errors.Is(issue.Err, ErrMissingDeep) {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Fatal("missing ticks were not reported after a cached positive estimate")
	}
}

type missingRecordedTicks struct {
	View
	key domain.PoolKey
}

func (v missingRecordedTicks) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	if key == v.key {
		return pool.DeepState{}, false
	}
	return v.View.Deep(key)
}
