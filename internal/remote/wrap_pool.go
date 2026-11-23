package remote

import (
	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/dex/wrap"
	"github.com/Rorical/Rosetta/internal/domain"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

// wrapNativeToken is the wrapped native token whose wrap pool the reactive
// policy enables (wrap_native), or the zero address without one.
func wrapNativeToken(cfg config.Config) (domain.Address, error) {
	if cfg.ReactiveConfig == "" {
		return domain.Address{}, nil
	}
	settings, err := loadReactiveSettings(cfg.ReactiveConfig, domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()})
	if err != nil || !settings.document.WrapNative {
		return domain.Address{}, err
	}
	return settings.wrapped, nil
}

// setWrapPool makes store hold the wrap pool of wrapped and no other: a pool
// snapshot written under another policy can carry one, or lack it. The zero
// address removes it. The pool has no state on chain, so it is neither read
// nor marked changed here; specs gains or loses it so that a reseed keeps
// the same pools.
func setWrapPool(store *poolstate.Store, specs []bootstrap.Spec, wrapped domain.Address) ([]bootstrap.Spec, error) {
	clean := len(store.Dirty()) == 0
	changed := false
	for _, p := range store.Pools() {
		if p.Key.Kind == domain.PoolKindWrap && p.Key.Address != wrapped {
			store.Remove(p.Key)
			changed = true
		}
	}
	if wrapped != (domain.Address{}) {
		if _, ok := store.Pool(wrap.Key(wrapped)); !ok {
			if err := store.Upsert(wrap.State(wrapped)); err != nil {
				return nil, err
			}
			changed = true
		}
	}
	if !changed {
		return specs, nil
	}
	if clean {
		store.ClearDirty()
	}
	return specsFor(store.Pools()), nil
}
