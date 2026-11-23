package remote

import (
	"context"

	"github.com/Rorical/Rosetta/internal/domain"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func (r *runtime) refreshTentativeDependencies(ctx context.Context, keys []domain.PoolKey) error {
	r.retireFlashResults()
	var view poolstate.View
	var version domain.StateVersion
	var ok bool
	if r.flashLogs != nil {
		if err := r.flashLogs.RefreshBase(ctx, r.store); err != nil {
			return r.resetTentative()
		}
		view, version, ok = r.flashLogs.View()
		keys = append(keys, r.flashLogs.AffectedPools()...)
	} else if r.fast != nil {
		if err := r.fast.RefreshBase(ctx, r.store); err != nil {
			return r.resetTentative()
		}
		var revisions map[domain.PoolKey]uint64
		view, version, revisions, ok = r.fast.View()
		for key := range revisions {
			keys = append(keys, key)
		}
	}
	if !ok {
		return nil
	}
	if r.flash.active {
		// Dependency changes retire quote tickets without changing the frame chain.
		version.PrefixHash = r.flashPrefix.Version.PrefixHash
		r.flashPrefix.Version = version
	}
	r.tentative = version
	return r.submit(ctx, view, keys, version)
}
