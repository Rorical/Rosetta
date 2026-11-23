package arb

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

var updateFrameGolden = flag.Bool("update-frame-golden", false, "rewrite testdata/frame_search_golden.txt")

// Ranking and sizing optimizations must not change any published number.
// The golden text records every coarse candidate and exact quote, in order,
// for the compiled, cached-incremental, generic and streamed worker paths.
func TestFrameSearchMatchesGolden(t *testing.T) {
	fixture := newFrameFixture(t)
	engine, opts := fixture.engine, fixture.opts
	var out strings.Builder
	section := func(name string) { fmt.Fprintf(&out, "# %s\n", name) }
	candidates := func(selection RankedSelection, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "considered %d issues %d\n", selection.considered, len(selection.issues))
		for _, c := range selection.candidates {
			fmt.Fprintf(&out, "%s %s in=%s net=%s\n", c.cycle.Start, routeText(c.cycle.Pools), c.input, c.net)
		}
	}
	opportunities := func(quotes []Opportunity) {
		for _, quote := range quotes {
			fmt.Fprintf(&out, "%s %s in=%s out=%s net=%s legs=", quote.Cycle.Start, routeText(quote.Cycle.Pools), quote.Input, quote.Output, quote.Net)
			for _, leg := range quote.Legs {
				fmt.Fprintf(&out, "%s>%s,", leg.AmountIn, leg.AmountOut)
			}
			out.WriteString("\n")
		}
	}

	var cache rankCache
	cache.bind(engine.compiled, 1)
	section("compiled cached full")
	candidates(engine.rankWithWorkers(t.Context(), fixture.base, fixture.all, opts, &cache, 2))
	section("compiled cached changed")
	candidates(engine.rankWithWorkers(t.Context(), fixture.changed, fixture.reserveSet, opts, &cache, 2))
	section("compiled cached restored")
	candidates(engine.rankWithWorkers(t.Context(), fixture.base, fixture.reserveSet, opts, &cache, 1))

	var cycles []graph.Cycle
	for index := range engine.table.Len() {
		cycles = append(cycles, engine.table.Cycle(index))
	}
	section("generic")
	candidates(engine.rankExplored(t.Context(), fixture.changed, cycles, fixture.all, opts, 2))

	section("evaluate ranked")
	result, err := engine.EvaluateRanked(t.Context(), fixture.base, fixture.all, opts)
	if err != nil {
		t.Fatal(err)
	}
	opportunities(result.Opportunities)

	// Streamed partial events race each other; their sorted contents do not.
	for _, changed := range []bool{false, true} {
		section(map[bool]string{false: "stream full", true: "stream changed"}[changed])
		initial, optimized, final := streamFrame(t, fixture, changed)
		fmt.Fprintf(&out, "selected %d\n", final.Selected)
		if final.ProbeOnly != 0 {
			t.Fatal("default options left paths unsized", final.ProbeOnly)
		}
		out.WriteString("initial\n")
		opportunities(initial)
		out.WriteString("optimized\n")
		opportunities(optimized)
	}

	const path = "testdata/frame_search_golden.txt"
	if *updateFrameGolden {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != string(want) {
		gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gotLines), len(wantLines)) {
			if gotLines[i] != wantLines[i] {
				t.Fatalf("line %d differs\n got: %s\nwant: %s", i+1, gotLines[i], wantLines[i])
			}
		}
		t.Fatalf("output has %d lines, golden %d", len(gotLines), len(wantLines))
	}
}

// streamFrame runs the full base frame and, when changed, a second frame in
// the same block, so lane caches carry over as they do in production.
func streamFrame(t *testing.T, fixture frameFixture, changed bool) (initial, optimized []Opportunity, final SearchResult) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	w, err := NewWorker(ctx, fixture.engine, fixture.opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); <-w.Done() }()
	frames := []SearchJob{{View: fixture.base, Dirty: fixture.all}}
	if changed {
		frames = append(frames, SearchJob{View: fixture.changed, Dirty: fixture.reserveSet})
	}
	for index, job := range frames {
		job.Incremental, job.GraphVersion = true, 1
		job.Version = domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 1, TxIndex: uint64(index)}
		if err := w.Submit(job); err != nil {
			t.Fatal(err)
		}
		initial, optimized = nil, nil
		for {
			event := <-w.Results()
			if event.Err != nil || !w.Rebase(&event) || !w.Accept(event) {
				t.Fatal("frame search", event.Err)
			}
			releaseSearch(event.Job)
			if !event.Partial {
				final = event.Result
				break
			}
			if event.Stage == QuoteInitial {
				initial = append(initial, event.Result.Opportunities...)
			} else {
				optimized = append(optimized, event.Result.Opportunities...)
			}
		}
	}
	order := func(a, b Opportunity) int {
		if order := strings.Compare(a.Cycle.Start.String(), b.Cycle.Start.String()); order != 0 {
			return order
		}
		return comparePoolSequences(a.Cycle.Pools, b.Cycle.Pools)
	}
	slices.SortFunc(initial, order)
	slices.SortFunc(optimized, order)
	return initial, optimized, final
}

func routeText(pools []domain.PoolKey) string {
	parts := make([]string, len(pools))
	for i, key := range pools {
		parts[i] = fmt.Sprintf("%x/%d", key.Address[:4], key.Kind)
	}
	return strings.Join(parts, "-")
}
