package pipeline

import (
	"slices"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// RetainDependencies runs only on a backend's state writer, after freshness
// acceptance. It preserves refreshed fees/ticks for following log updates.
// SourceFast state is never retained in the canonical store.
func RetainDependencies(store *state.Store, view arb.View, version domain.StateVersion, shallowKeys, deepKeys []domain.PoolKey) ([]pool.State, []state.DeepDelta, error) {
	if version.Source != domain.SourceStandard || store.Block() != version.Anchor {
		return nil, nil, nil
	}
	var shallow []pool.State
	var deep []state.DeepDelta
	slices.SortFunc(shallowKeys, domain.ComparePoolKeys)
	for _, key := range slices.Compact(shallowKeys) {
		updated, ok := view.Pool(key)
		if !ok {
			continue
		}
		current, ok := store.Pool(key)
		if !ok {
			continue
		}
		sameReverse := current.FeeOneForZero == nil && updated.FeeOneForZero == nil || current.FeeOneForZero != nil && updated.FeeOneForZero != nil && *current.FeeOneForZero == *updated.FeeOneForZero
		if updated.Slipstream == nil && current.Fee == updated.Fee && current.ProtocolFee == updated.ProtocolFee && sameReverse {
			continue
		}
		// Only fee dependencies are adopted; the writer owns price/liquidity logs.
		current.Fee, current.ProtocolFee, current.FeeOneForZero = updated.Fee, updated.ProtocolFee, updated.FeeOneForZero
		if updated.Slipstream != nil {
			current.Slipstream = updated.Slipstream
		}
		if err := store.Upsert(current); err != nil {
			return nil, nil, err
		}
		shallow = append(shallow, current)
	}
	slices.SortFunc(deepKeys, domain.ComparePoolKeys)
	for _, key := range slices.Compact(deepKeys) {
		updated, ok := view.Deep(key)
		if !ok || updated.LoadedBlock != store.Block() {
			continue
		}
		low, high, loaded, ok := store.DeepStatus(key)
		if ok && loaded == updated.LoadedBlock && low == updated.WindowMin && high == updated.WindowMax {
			continue
		}
		if err := store.SetDeep(key, updated); err != nil {
			return nil, nil, err
		}
		deep = append(deep, state.DeepDelta{Key: key, State: updated})
	}
	// These refreshes do not introduce another scheduling trigger. Dynamic pools
	// already enter every canonical dirty set; price events retain their own set.
	store.ClearDirty()
	return shallow, deep, nil
}
