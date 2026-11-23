package arb

import (
	"context"
	"errors"
	"math/big"

	"github.com/Rorical/Rosetta/internal/math"
)

func (s *amountSearch) quoteAt(ctx context.Context, input *big.Int) (*amountSample, error) {
	work, e, view, candidate := s.work, s.engine, s.view, s.candidate
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
		// A larger input than a pool can fill is a sizing bound, not a stale
		// dependency: it neither requests ticks nor blocks path caching.
		capacity := errors.Is(err, math.ErrCapacity)
		if capacity {
			s.limits.Capacity++
		} else if errors.Is(err, math.ErrOutOfWindow) {
			s.limits.OutOfWindow++
		}
		var issue *QuoteIssue
		if !capacity && errors.As(err, &issue) {
			s.issues = appendIssue(s.issues, *issue)
		}
		if s.monotone && (capacity || errors.Is(err, math.ErrOutOfWindow) || errors.Is(err, math.ErrMissingTick)) {
			work.ceiling.Set(input)
			work.hasCeiling = true
		}
		return sample, nil
	}
	sample.net.Set(opportunity.Net)
	sample.ok = true
	if !s.quoted || sample.net.Cmp(s.best.Net) > 0 || (sample.net.Cmp(s.best.Net) == 0 && input.Cmp(s.best.Input) < 0) {
		if !s.quoted {
			s.best = Opportunity{Cycle: candidate.cycle, Input: new(big.Int), Net: new(big.Int)}
		}
		s.best.Input.Set(input)
		s.best.Net.Set(&sample.net)
		s.best.Output = opportunity.Output
		s.best.Legs = append(s.best.Legs[:0], opportunity.Legs...)
		s.best.Legs[0].AmountIn = s.best.Input
		s.quoted = true
	}
	if s.positive != nil && opportunity.Net.Sign() > 0 {
		if err := s.positive(opportunity); err != nil {
			return nil, err
		}
		s.positive = nil
	}
	return sample, nil
}
