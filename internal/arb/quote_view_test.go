package arb

import (
	"errors"
	"math/big"
	"reflect"
	"testing"
)

func TestOptimizeReadsTickSnapshotOnceAndKeepsOwnedResults(t *testing.T) {
	engine, view, selection := exactBenchmarkFixture(t)
	first, err := engine.Optimize(t.Context(), view, selection)
	if err != nil || len(first.Opportunities) != 1 {
		t.Fatal(first, err)
	}
	if view.poolReads != 2 || view.deepReads != 2 {
		t.Fatalf("repeated snapshot copies: shallow=%d deep=%d", view.poolReads, view.deepReads)
	}
	uncached, issues, ok, err := engine.optimizeCycle(t.Context(), view, selection.candidates[0])
	actual := first.Opportunities[0]
	if err != nil || !ok || !reflect.DeepEqual(issues, first.Issues) || actual.Input.Cmp(uncached.Input) != 0 || actual.Net.Cmp(uncached.Net) != 0 || !reflect.DeepEqual(actual.Legs, uncached.Legs) {
		t.Fatalf("snapshot reuse changed amount search: cached input=%v net=%v issues=%v; uncached input=%v net=%v issues=%v error=%v", actual.Input, actual.Net, first.Issues, uncached.Input, uncached.Net, issues, err)
	}
	saved := first.Opportunities[0].Net.String()
	for range 4 {
		again, err := engine.Optimize(t.Context(), view, selection)
		if err != nil || !reflect.DeepEqual(again, first) {
			t.Fatal("cache reuse changed optimization", err)
		}
		again.Opportunities[0].Net.SetInt64(0)
	}
	if first.Opportunities[0].Net.String() != saved {
		t.Fatal("previous result aliases reused cache")
	}
	// A fresh invocation must observe a refreshed snapshot, including recovery
	// from a cached miss during an earlier same-root dependency retry.
	key := selection.Pools()[0]
	deep := view.deep[key]
	delete(view.deep, key)
	missing, err := engine.Optimize(t.Context(), view, selection)
	if err != nil || len(missing.Issues) != 1 || !errors.Is(missing.Issues[0].Err, ErrMissingDeep) {
		t.Fatal("missing dependency disappeared", missing, err)
	}
	view.deep[key] = deep
	recovered, err := engine.Optimize(t.Context(), view, selection)
	if err != nil || !reflect.DeepEqual(first, recovered) {
		t.Fatal("cached miss survived into refreshed snapshot", err)
	}
	if view.states[key].Liquidity.Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Fatal("quote cache mutated the source snapshot")
	}
}
