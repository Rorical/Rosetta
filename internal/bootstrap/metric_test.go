package bootstrap

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// A MetricOmm pool is registered without a read: the runtime reads its words,
// extension state and quote together at a canonical block. Words left in the
// spec by a previous run are not carried into the seed.
func TestSeedRegistersMetricOmmPoolStaleWithoutReads(t *testing.T) {
	registration := &pool.MetricState{
		Token0Scale: big.NewInt(1), Token1Scale: big.NewInt(1_000_000_000_000), LowestBin: -20, HighestBin: 19,
		Oracle: domain.Address{0xe8}, FeedIndex: 1, OracleSlot: domain.Hash{1}, QuoteExtension: domain.Address{0xf1}, QuoteExtensionSlot: domain.Hash{2},
		Words: make([]domain.Hash, 42), BidX64: big.NewInt(1), AskX64: big.NewInt(2),
	}
	spec := Spec{Address: domain.Address{0x2b}, Kind: domain.PoolKindMetricOmm, Token0: testToken0, Token1: testToken1, Metric: registration}
	caller := newFakeCaller()
	store := state.New()
	ref := domain.BlockRef{Number: 5, Hash: domain.Hash{5}}
	if err := Seed(t.Context(), caller, store, []Spec{spec}, ref, 1); err != nil {
		t.Fatal(err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("%d calls for a registration", len(caller.calls))
	}
	p, ok := store.Pool(domain.PoolKey{Address: spec.Address, Kind: domain.PoolKindMetricOmm})
	if !ok || p.Token0 != testToken0 || p.Token1 != testToken1 || p.Metric == nil {
		t.Fatalf("pool not registered: %+v", p)
	}
	if !p.Metric.Stale || p.Metric.Words != nil || p.Metric.BidX64 != nil || p.Metric.Oracle != registration.Oracle || p.Metric.HighestBin != 19 {
		t.Fatalf("a seeded pool must be stale and unread: %+v", p.Metric)
	}

	for name, mutate := range map[string]func(*Spec){
		"no registration": func(s *Spec) { s.Metric = nil },
		"no oracle":       func(s *Spec) { m := *s.Metric; m.Oracle = domain.Address{}; s.Metric = &m },
		"fee":             func(s *Spec) { s.Fee = 500 },
		"same tokens":     func(s *Spec) { s.Token1 = s.Token0 },
	} {
		broken := spec
		mutate(&broken)
		if err := Seed(t.Context(), caller, state.New(), []Spec{broken}, ref, 1); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
