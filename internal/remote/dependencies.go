package remote

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
)

type dependencyView struct {
	arb.View
	shallow  map[domain.PoolKey]pool.State
	deep     map[domain.PoolKey]pool.DeepState
	rejected map[domain.PoolKey]bool
}

func (v *dependencyView) Pool(key domain.PoolKey) (pool.State, bool) {
	if v.rejected[key] {
		return pool.State{}, false
	}
	if state, ok := v.shallow[key]; ok {
		return state.Copy(), true
	}
	return v.View.Pool(key)
}
func (v *dependencyView) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	if v.rejected[key] {
		return pool.DeepState{}, false
	}
	if state, ok := v.deep[key]; ok {
		return state.Copy(), true
	}
	return v.View.Deep(key)
}

// SharedDeep is Deep for the quoter, which only reads (arb.SharedDeepView).
func (v *dependencyView) SharedDeep(key domain.PoolKey) (pool.DeepState, bool) {
	if v.rejected[key] {
		return pool.DeepState{}, false
	}
	if state, ok := v.deep[key]; ok {
		return state, true
	}
	if shared, ok := v.View.(arb.SharedDeepView); ok {
		return shared.SharedDeep(key)
	}
	return v.View.Deep(key)
}

// floor is the configured word radius; each pool loads at least its coverage radius.
func prepareDependencies(reader *snapshot, table arb.CycleTable, floor int16, exact bool) func(context.Context, arb.View, []domain.PoolKey) (arb.View, []arb.QuoteIssue, error) {
	return func(ctx context.Context, original arb.View, keys []domain.PoolKey) (arb.View, []arb.QuoteIssue, error) {
		if !exact {
			keys = graph.Dependencies(table, keys)
		}
		view := &dependencyView{View: original, shallow: make(map[domain.PoolKey]pool.State), deep: make(map[domain.PoolKey]pool.DeepState), rejected: make(map[domain.PoolKey]bool)}
		block := "0x" + strconv.FormatUint(reader.block.Ref.Number, 16)
		var issues []arb.QuoteIssue
		for _, key := range keys {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			current, exists := view.Pool(key)
			if !exists {
				continue
			}
			var err error
			// A scheduled fee changes only by a setter; the follower's
			// schedule keeps the store current (feeSchedule).
			if !exact && pipeline.DynamicFee(key.Kind) && !scheduledFeeKind(key.Kind, key.Hooks) {
				var updated pool.State
				switch key.Kind {
				case domain.PoolKindVelodromeV2:
					updated, err = velodromev2.LoadFee(ctx, reader, current, block)
				case domain.PoolKindCamelotV3:
					updated, err = camelotv3.LoadFee(ctx, reader, current, block)
				case domain.PoolKindSlipstream:
					updated, err = slipstream.LoadFee(ctx, reader, current, block)
				case domain.PoolKindV4:
					updated, err = uniswapv4.LoadFee(ctx, reader, current, block)
				}
				if err == nil {
					view.shallow[key] = updated
				}
			}
			if exact && key.Kind.UsesTicks() {
				words := pool.WindowRadius(current.TickSpacing, floor)
				if deep, ok := view.Deep(key); ok && coversDeepWindow(current, deep, words) {
					continue
				}
				var deep pool.DeepState
				switch key.Kind {
				case domain.PoolKindV3, domain.PoolKindSlipstream, domain.PoolKindSolidlyV3:
					deep, err = (&uniswapv3.DeepLoader{Caller: reader}).LoadDeep(ctx, current, block, words)
				case domain.PoolKindCamelotV3:
					deep, err = camelotv3.LoadDeep(ctx, reader, current, block, words)
				case domain.PoolKindV4:
					deep, err = uniswapv4.LoadDeep(ctx, reader, current, block, words)
				default:
					err = fmt.Errorf("unsupported deep pool")
				}
				if err == nil {
					deep.LoadedBlock = reader.block.Ref
					view.deep[key] = deep
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				view.rejected[key] = true
				issues = append(issues, arb.QuoteIssue{Pool: key, Err: err})
			}
		}
		return view, issues, nil
	}
}
