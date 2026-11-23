package arb

import (
	"context"
	"errors"
	"fmt"
	stdmath "math"
	"math/big"
	"math/bits"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func (e *Engine) rankCompiled(ctx context.Context, s *compiledSnapshot, indices []int, limit int, cache *rankCache, diverse bool) (RankedSelection, error) {
	selection := RankedSelection{}
	ranked := make(map[domain.Address]*rankBucket)
	work := rankWorkspaces.Get().(*rankWorkspace)
	defer work.release()
	for _, index := range indices {
		if err := ctx.Err(); err != nil {
			return selection, err
		}
		hops := s.layout.path(index)
		work.resize(len(hops))
		work.legs = slices.Grow(work.legs[:0], len(hops))[:len(hops)]
		complete, issue := true, false
		var revision uint64
		if cache != nil {
			input := cache.inputs[index]
			complete = input.missing == 0
			issue = input.issues > 0
			revision = input.revision
		} else {
			for _, hop := range hops {
				id, direction := hop/2, hop%2
				if !s.pools[id].ok {
					complete = false
					break
				}
				issue = issue || s.pools[id].errs[direction] != nil
			}
		}

		if !complete {
			continue
		}
		if cache != nil && len(hops) <= 16 {
			mask := cache.rotations[index]
			selection.considered += bits.OnesCount16(mask)
			if mask == 0 || (cache.fundingValid && cache.negative[index] == revision && revision != 0) {
				continue
			}
			for mask != 0 {
				offset := bits.TrailingZeros16(mask)
				mask &= mask - 1
				hop := hops[offset]
				tokenID := s.layout.pairs[hop/2][hop%2]
				funding := s.funding[tokenID]
				if !funding.valid {
					return selection, fmt.Errorf("%w: funding bounds for %s", ErrInvalidConfig, s.layout.tokens[tokenID])
				}
				work.funded = append(work.funded, fundedRotation{offset: offset, token: s.layout.tokens[tokenID], bounds: funding.bounds, low: funding.low, high: funding.high})
			}
		} else {
			for offset, hop := range hops {
				tokenID := s.layout.pairs[hop/2][hop%2]
				funding := s.funding[tokenID]
				if !funding.ok {
					continue
				}
				token := s.layout.tokens[tokenID]
				if !funding.valid {
					return selection, fmt.Errorf("%w: funding bounds for %s", ErrInvalidConfig, token)
				}
				work.funded = append(work.funded, fundedRotation{offset: offset, token: token, bounds: funding.bounds, low: funding.low, high: funding.high})
			}
			selection.considered += len(work.funded)
		}
		if len(work.funded) == 0 || (cache != nil && cache.negative[index] == revision && revision != 0) {
			continue
		}

		if !issue && !work.compiledCanProfit(s, hops) {
			if cache != nil {
				cache.negative[index] = revision
			}
			continue
		}
		for _, rotation := range work.funded {
			candidate := rankedCycle{path: index, rotation: rotation.offset, revision: revision, bounds: rotation.bounds}
			bucket := ranked[rotation.token]
			bound := modelCycleUpper(work.intervals, rotation.offset, rotation.low, rotation.high)
			if !issue && !stdmath.IsInf(bound, 1) {
				ceilFloat(&work.upper, bound)
				if !rotation.bounds.clearsFloor(&work.upper) || (bucket != nil && bucket.rejectUpper(&work.upper, s.layout, hops, limit)) {
					continue
				}
			}
			input, net, err := work.compiledEstimate(s, hops, index, rotation, revision, cache)
			if err != nil {
				var issue *QuoteIssue
				if errors.As(err, &issue) {
					selection.issues = appendIssue(selection.issues, *issue)
				}
				continue
			}
			candidate.input, candidate.net = input, net

			if !rotation.bounds.clearsFloor(candidate.net) {
				continue
			}
			keys := work.rotations[rotation.offset*len(hops) : (rotation.offset+1)*len(hops)]
			for i := range hops {
				keys[i] = s.layout.keys[hops[(i+rotation.offset)%len(hops)]/2]
			}
			candidate.cycle = graph.Cycle{Start: rotation.token, Pools: keys, Len: len(keys)}
			if e.phantoms.excludes(keys) {
				continue
			}
			bucket = ranked[candidate.cycle.Start]
			if bucket == nil {
				bucket = &rankBucket{diverse: diverse}
				ranked[candidate.cycle.Start] = bucket
			}
			bucket.admit(candidate, limit, true)
		}
	}
	for _, bucket := range ranked {
		selection.candidates = append(selection.candidates, bucket.members()...)
	}
	return selection, nil
}

func (w *rankWorkspace) compiledCanProfit(s *compiledSnapshot, hops []uint32) bool {
	upper, lower := 1.0, 1.0
	approximate := false
	for i, hop := range hops {
		leg := s.pools[hop/2].legs[hop%2]
		approximate = approximate || leg.approximate
		w.legs[i] = leg.model
		w.intervals[i] = leg.interval
		upper = stdmath.Nextafter(upper*leg.rateUpper, stdmath.Inf(1))
		lower = roundDown(lower * leg.rateLower)
	}
	if approximate {
		return true
	}
	return marginProfitable(upper, lower, w.legs)
}

func (w *rankWorkspace) compiledEstimate(s *compiledSnapshot, hops []uint32, index int, rotation fundedRotation, revision uint64, cache *rankCache) (*big.Int, *big.Int, error) {
	key := estimateKey{index, rotation.offset}
	if cache != nil && cache.estimates.load(key, revision, cache.fundingRevision, cache.pass, &w.input, &w.net) {
		return &w.input, &w.net, nil
	}
	// Dependency failures take precedence over invalid model values, matching
	// the generic estimator that collects all legs before doing arithmetic.
	var modelErr error
	for i := range hops {
		hop := hops[(i+rotation.offset)%len(hops)]
		if err := s.pools[hop/2].errs[hop%2]; err != nil {
			return nil, nil, &QuoteIssue{Pool: s.layout.keys[hop/2], Err: err}
		}
		if modelErr == nil {
			modelErr = s.pools[hop/2].legs[hop%2].curveErr
		}
	}
	if modelErr != nil {
		return nil, nil, modelErr
	}
	if !w.curveReady || w.curveOffset != rotation.offset {
		w.prepareCurve(s, hops, rotation.offset, cache)
		w.curveReady = true
		w.curveOffset = rotation.offset
	}

	if err := w.curve.EstimateInto(&w.input, &w.net, rotation.bounds.Min, rotation.bounds.Max); err != nil {
		return nil, nil, err
	}
	if cache != nil {
		cache.estimates.store(key, revision, cache.fundingRevision, cache.pass, &w.input, &w.net)
	}
	return &w.input, &w.net, nil
}

// Every directed chunk is reusable, including middle and suffix chunks.
// The final composition remains exact; no rounded intermediate reserves occur.
func (w *rankWorkspace) prepareCurve(s *compiledSnapshot, hops []uint32, offset int, cache *rankCache) {
	for position := 0; position < len(hops); {
		length := min(3, len(hops)-position)
		if length == 1 {
			hop := hops[(offset+position)%len(hops)]
			if position == 0 {
				w.curve.Curve.Set(&s.pools[hop/2].legs[hop%2].curve)
			} else {
				w.curve.Append(&s.pools[hop/2].legs[hop%2].curve)
			}
		} else {
			w.prepareSegment(s, hops, (offset+position)%len(hops), length, cache)
			if position == 0 {
				w.curve.Curve.Set(&w.segmentCurve.Curve)
			} else {
				w.curve.Append(&w.segmentCurve.Curve)
			}
		}
		position += length
	}
}
