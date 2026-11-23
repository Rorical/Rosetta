package arb

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/math"
)

// Frozen pre-pipeline algorithm: compares pauses at every phase against the
// original amount sequence, including final legs, tie breaking and callbacks.
func TestResumableSizingMatchesRecordedReference(t *testing.T) {
	engine, view, dirty, opts := recordedSearchFixture(t)
	selection, err := engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range selection.candidates {
		want, issues, ok, err := engine.referenceAmountSearch(t.Context(), view, candidate, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, budget := range []int{1, 2, 8, 108} {
			search := newAmountSearch(engine, view, candidate, nil)
			for {
				done, err := search.step(t.Context(), budget)
				if err != nil {
					t.Fatal(err)
				}
				if done {
					break
				}
			}
			if ok != search.quoted || !reflect.DeepEqual(want, search.best) || !reflect.DeepEqual(issues, search.issues) {
				t.Fatal("yield changed recorded amount optimization", budget)
			}
			search.close()
		}
	}
}

func (e *Engine) referenceAmountSearch(ctx context.Context, view View, candidate rankedCycle, positive func(Opportunity) error) (Opportunity, []QuoteIssue, bool, error) {
	points := sizingPoints(candidate.bounds, candidate.input, e.notional(candidate.cycle.Start))
	work := sizingWorkspaces.Get().(*sizingWorkspace)
	defer work.release()
	work.legs = slices.Grow(work.legs, len(candidate.cycle.Pools))[:len(candidate.cycle.Pools)]
	monotone := e.monotoneCycle(view, candidate.cycle)
	var best Opportunity
	var issues []QuoteIssue
	quoted := false
	quoteAt := func(input *big.Int) (*amountSample, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := amountKey(input)
		if index, found := work.index[key]; found {
			return &work.samples[index], nil
		}
		if work.hasCeiling && input.Cmp(&work.ceiling) >= 0 {
			return nil, nil
		}
		sample := &work.samples[work.count]
		work.index[key] = work.count
		work.count++
		sample.input.Set(input)
		sample.ok = false
		opportunity, err := e.quoteInto(ctx, view, candidate.cycle, input, work.legs[:0], &work.net)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var issue *QuoteIssue
			if errors.As(err, &issue) {
				issues = appendIssue(issues, *issue)
			}
			if monotone && (errors.Is(err, math.ErrOutOfWindow) || errors.Is(err, math.ErrMissingTick)) {
				work.ceiling.Set(input)
				work.hasCeiling = true
			}
			return sample, nil
		}
		sample.net.Set(opportunity.Net)
		sample.ok = true
		if !quoted || sample.net.Cmp(best.Net) > 0 || (sample.net.Cmp(best.Net) == 0 && input.Cmp(best.Input) < 0) {
			if !quoted {
				best = Opportunity{Cycle: candidate.cycle, Input: new(big.Int), Net: new(big.Int)}
			}
			best.Input.Set(input)
			best.Net.Set(&sample.net)
			best.Output = opportunity.Output
			best.Legs = append(best.Legs[:0], opportunity.Legs...)
			best.Legs[0].AmountIn = best.Input
			quoted = true
		}
		if positive != nil && opportunity.Net.Sign() > 0 {
			if err := positive(opportunity); err != nil {
				return nil, err
			}
			positive = nil
		}
		return sample, nil
	}
	// The model's optimum is already in the bounded grid. Try it first to
	// expose a usable quote without adding samples or reducing final sizing.
	if positive != nil && candidate.input != nil {
		if _, err := quoteAt(candidate.input); err != nil {
			return Opportunity{}, issues, false, err
		}
	}
	for _, point := range points {
		if _, err := quoteAt(point); err != nil {
			return Opportunity{}, issues, false, err
		}
	}
	if !quoted {
		return Opportunity{}, issues, false, nil
	}
	index, _ := slices.BinarySearchFunc(points, best.Input, func(a, b *big.Int) int { return a.Cmp(b) })
	left, right := &work.left, &work.right
	left.Set(points[max(0, index-1)])
	right.Set(points[min(len(points)-1, index+1)])
	for range 32 {
		if monotone && work.cannotImprove(best.Net) {
			return best, issues, true, nil
		}
		work.width.Sub(right, left)
		if work.width.Cmp(big.NewInt(8)) <= 0 {
			break
		}
		work.third.Quo(&work.width, big.NewInt(3))
		work.low.Add(left, &work.third)
		work.high.Sub(right, &work.third)
		a, err := quoteAt(&work.low)
		if err != nil {
			return Opportunity{}, issues, false, err
		}
		b, err := quoteAt(&work.high)
		if err != nil {
			return Opportunity{}, issues, false, err
		}
		// Missing ticks bound larger inputs only for certified monotone curves.
		if b == nil || !b.ok || (a != nil && a.ok && a.net.Cmp(&b.net) >= 0) {
			right.Set(&work.high)
		} else {
			left.Set(&work.low)
		}
	}
	for count := 0; left.Cmp(right) <= 0 && count < 8; count++ {
		if _, err := quoteAt(left); err != nil {
			return Opportunity{}, issues, false, err
		}
		left.Add(left, big.NewInt(1))
	}
	return best, issues, true, nil
}
