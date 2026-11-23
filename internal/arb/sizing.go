package arb

import (
	"context"
	"math/big"
	"slices"
)

// Amount sizing preserves the logarithmic grid, 32 refinement pairs and eight
// final integer neighbours. The same state machine also runs in bounded quanta
// on live workers; yielding never changes the amount sequence or pruning proof.
func (e *Engine) optimizeCycle(ctx context.Context, view View, candidate rankedCycle) (Opportunity, []QuoteIssue, bool, error) {
	return e.optimizeCycleObserved(ctx, view, candidate, nil, &QuoteLimits{})
}

// limits accumulates the search's rejected samples, including on error.
func (e *Engine) optimizeCycleObserved(ctx context.Context, view View, candidate rankedCycle, positive func(Opportunity) error, limits *QuoteLimits) (Opportunity, []QuoteIssue, bool, error) {
	search := newAmountSearch(e, view, candidate, positive)
	defer search.close()
	defer func() { limits.add(search.limits) }()
	for {
		done, err := search.step(ctx, 8)
		if err != nil {
			return Opportunity{}, search.issues, false, err
		}
		if done {
			return search.best, search.issues, search.quoted, nil
		}
	}
}

// An amountSearch has exactly one worker owner at a time, including its scratch
// integers and partial refinement pair. Published quotes must be owned copies.
type sizingPhase uint8

const (
	sizingProbe sizingPhase = iota
	sizingGrid
	sizingLow
	sizingHigh
	sizingNeighbours
)

type amountSearch struct {
	engine                 *Engine
	view                   View
	candidate              rankedCycle
	work                   *sizingWorkspace
	points                 []*big.Int
	best                   Opportunity
	issues                 []QuoteIssue
	limits                 QuoteLimits
	positive               func(Opportunity) error
	quoted, monotone       bool
	phase                  sizingPhase
	point, pair, neighbour int
	low                    *amountSample
}

func newAmountSearch(e *Engine, view View, candidate rankedCycle, positive func(Opportunity) error) *amountSearch {
	work := sizingWorkspaces.Get().(*sizingWorkspace)
	work.legs = slices.Grow(work.legs, len(candidate.cycle.Pools))[:len(candidate.cycle.Pools)]
	return &amountSearch{engine: e, view: view, candidate: candidate, positive: positive, work: work, monotone: e.monotoneCycle(view, candidate.cycle)}
}

func (s *amountSearch) close() { s.work.release() }

// step bounds attempts, including memo hits, so no route can monopolize a
// worker. It may yield between the low and high samples of a refinement pair.
func (s *amountSearch) step(ctx context.Context, budget int) (bool, error) {
	w := s.work
	for used := 0; used < budget; {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var input *big.Int
		switch s.phase {
		case sizingProbe:
			s.phase = sizingGrid
			if s.positive != nil && s.candidate.input != nil {
				input = s.candidate.input
			}
		case sizingGrid:
			if s.points == nil {
				// Built on first use: a probe-only exact task never reaches the grid.
				s.points = sizingPoints(s.candidate.bounds, s.candidate.input, s.engine.notional(s.candidate.cycle.Start))
			}
			if s.point < len(s.points) {
				input = s.points[s.point]
				s.point++
			} else {
				if !s.quoted {
					return true, nil
				}
				index, _ := slices.BinarySearchFunc(s.points, s.best.Input, func(a, b *big.Int) int { return a.Cmp(b) })
				w.left.Set(s.points[max(0, index-1)])
				w.right.Set(s.points[min(len(s.points)-1, index+1)])
				s.phase = sizingLow
			}
		case sizingLow:
			if s.pair == 32 {
				s.phase = sizingNeighbours
				continue
			}
			if s.monotone && w.cannotImprove(s.best.Net) {
				return true, nil
			}
			w.width.Sub(&w.right, &w.left)
			if w.width.Cmp(big.NewInt(8)) <= 0 {
				s.phase = sizingNeighbours
				continue
			}
			w.third.Quo(&w.width, big.NewInt(3))
			w.low.Add(&w.left, &w.third)
			w.high.Sub(&w.right, &w.third)
			sample, err := s.quoteAt(ctx, &w.low)
			if err != nil {
				return false, err
			}
			s.low = sample
			s.phase = sizingHigh
			used++
			continue
		case sizingHigh:
			high, err := s.quoteAt(ctx, &w.high)
			if err != nil {
				return false, err
			}
			if high == nil || !high.ok || (s.low != nil && s.low.ok && s.low.net.Cmp(&high.net) >= 0) {
				w.right.Set(&w.high)
			} else {
				w.left.Set(&w.low)
			}
			s.phase = sizingLow
			s.pair++
			used++
			continue
		case sizingNeighbours:
			if s.neighbour == 8 || w.left.Cmp(&w.right) > 0 {
				return true, nil
			}
			if _, err := s.quoteAt(ctx, &w.left); err != nil {
				return false, err
			}
			w.left.Add(&w.left, big.NewInt(1))
			s.neighbour++
			used++
			continue
		}
		if input != nil {
			if _, err := s.quoteAt(ctx, input); err != nil {
				return false, err
			}
			used++
		}
	}
	return false, nil
}

func sizingPoints(bounds InputBounds, hints ...*big.Int) []*big.Int {
	points := []*big.Int{new(big.Int).Set(bounds.Min), new(big.Int).Set(bounds.Max)}
	span := bounds.Max.BitLen() - bounds.Min.BitLen() + 1
	for i := 1; i <= 32; i++ {
		point := new(big.Int).Lsh(bounds.Min, uint(span*i/32))
		if point.Cmp(bounds.Max) < 0 {
			points = append(points, point)
		}
	}
	for _, hint := range hints {
		if hint != nil && hint.Cmp(bounds.Min) >= 0 && hint.Cmp(bounds.Max) <= 0 {
			points = append(points, new(big.Int).Set(hint))
		}
	}
	slices.SortFunc(points, func(a, b *big.Int) int { return a.Cmp(b) })
	return slices.CompactFunc(points, func(a, b *big.Int) bool { return a.Cmp(b) == 0 })
}
