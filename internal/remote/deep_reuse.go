package remote

import (
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Completed standard searches enrich the writer's cache for subsequent blocks.
// Fast views remain private: their ticks include an unconfirmed prefix.
func (r *runtime) retainDependencies(event arb.SearchEvent) error {
	if event.Err != nil || event.Job.Version.Source != domain.SourceStandard || !r.Current(event.Job.Version) {
		return nil
	}
	var keys, shallowKeys []domain.PoolKey
	for view := event.Job.View; ; {
		dependencies, ok := view.(*dependencyView)
		if !ok {
			break
		}
		for key := range dependencies.shallow {
			shallowKeys = append(shallowKeys, key)
		}
		for key := range dependencies.deep {
			keys = append(keys, key)
		}
		view = dependencies.View
	}
	shallow, deep, err := pipeline.RetainDependencies(r.store, event.Job.View, event.Job.Version, shallowKeys, keys)
	if err != nil {
		return err
	}
	if len(shallow) > 0 || len(deep) > 0 {
		return r.capture.Dependencies(r.store.Block(), shallow, deep)
	}
	return nil
}

// A fresh, log-maintained window already contains every word the loader would
// read. Unknown/stale windows and moves beyond those words still require RPC.
func coversDeepWindow(state pool.State, deep pool.DeepState, radius int16) bool {
	if state.TickSpacing <= 0 || radius < 1 || deep.WindowMin == 0 && deep.WindowMax == 0 {
		return false
	}
	if state.Key.Kind == domain.PoolKindCamelotV3 {
		return false
	}
	width := int64(state.TickSpacing) * 256
	word := int64(state.Tick) / width
	if int64(state.Tick) < 0 && int64(state.Tick)%width != 0 {
		word--
	}
	low := max(int64(math.MinTick), (word-int64(radius))*width)
	high := min(int64(math.MaxTick), (word+int64(radius)+1)*width-1)
	return int64(deep.WindowMin) <= low && int64(deep.WindowMax) >= high
}
