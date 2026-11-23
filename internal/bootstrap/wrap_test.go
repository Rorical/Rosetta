package bootstrap

import (
	"errors"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// The wrap of the native token is seeded without a read, so a reseed after a
// reorg keeps it, and only as the pool between native currency and the token
// at its own address.
func TestSeedRegistersTheWrapPoolWithoutReads(t *testing.T) {
	wrapped := domain.Address{0x82}
	spec := Spec{Address: wrapped, Kind: domain.PoolKindWrap, Token1: wrapped}
	caller := newFakeCaller()
	store := state.New()
	ref := domain.BlockRef{Number: 5, Hash: domain.Hash{5}}
	if err := Seed(t.Context(), caller, store, []Spec{spec}, ref, 1); err != nil {
		t.Fatal(err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("%d calls for a wrap", len(caller.calls))
	}
	p, ok := store.Pool(domain.PoolKey{Address: wrapped, Kind: domain.PoolKindWrap})
	if !ok || p.Token0 != (domain.Address{}) || p.Token1 != wrapped || p.Fee != 0 {
		t.Fatalf("pool not registered: %+v", p)
	}
	if _, ok := store.Deep(p.Key); ok || len(store.DeepStale()) != 0 {
		t.Fatal("a wrap has no deep state to load")
	}

	for name, mutate := range map[string]func(*Spec){
		"token0 not native":     func(s *Spec) { s.Token0 = domain.Address{9} },
		"token1 not the target": func(s *Spec) { s.Token1 = domain.Address{9} },
		"fee":                   func(s *Spec) { s.Fee = 500 },
		"tick spacing":          func(s *Spec) { s.TickSpacing = 10 },
		"pool ID":               func(s *Spec) { s.PoolID = domain.Hash{1} },
		"hooks":                 func(s *Spec) { s.Hooks = domain.Address{1} },
		"shallow":               func(s *Spec) { s.Shallow = true },
		"metric":                func(s *Spec) { s.Metric = &pool.MetricState{} },
	} {
		broken := spec
		mutate(&broken)
		if err := Seed(t.Context(), caller, state.New(), []Spec{broken}, ref, 1); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
