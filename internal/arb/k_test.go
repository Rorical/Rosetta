package arb

import (
	"context"
	"errors"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestEngineKDefaultsToConstructionValue(t *testing.T) {
	table := mustTable(t, 2, testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000))
	engine := mustEngine(t, table, 1_000, 7, identityQuoter())
	if got := engine.K(); got != 7 {
		t.Fatalf("K() = %d, want 7", got)
	}
}

func TestEngineSetKValidates(t *testing.T) {
	table := mustTable(t, 2, testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000))
	engine := mustEngine(t, table, 1_000, 7, identityQuoter())

	for _, k := range []int{0, -1} {
		if err := engine.SetK(k); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("SetK(%d) error = %v, want ErrInvalidConfig", k, err)
		}
		if got := engine.K(); got != 7 {
			t.Fatalf("K() = %d after rejected SetK(%d), want 7", got, k)
		}
	}
	if err := engine.SetK(3); err != nil {
		t.Fatalf("SetK(3): %v", err)
	}
	if got := engine.K(); got != 3 {
		t.Fatalf("K() = %d, want 3", got)
	}
}

// TestEngineSetKChangesCandidateCount proves a later Evaluate observes the new
// cap rather than the construction value.
func TestEngineSetKChangesCandidateCount(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x01, 0x02, 1_000_000, 2_000_000)
	poolC := testV2State(0x30, 0x01, 0x02, 1_000_000, 2_000_000)
	table := mustTable(t, 2, poolA, poolB, poolC)
	view := newFakeView().add(poolA).add(poolB).add(poolC)
	dirty := []domain.PoolKey{poolA.Key}
	engine := mustEngine(t, table, 1_000, 1, identityQuoter())

	evaluate := func() int {
		t.Helper()
		opportunities, err := engine.Evaluate(context.Background(), view, dirty)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		return len(opportunities)
	}
	if got := evaluate(); got != 1 {
		t.Fatalf("opportunities at K=1 = %d, want 1", got)
	}
	if err := engine.SetK(2); err != nil {
		t.Fatalf("SetK(2): %v", err)
	}
	if got := evaluate(); got != 2 {
		t.Fatalf("opportunities at K=2 = %d, want 2", got)
	}
}

func TestEngineWithGasCopiesCurrentK(t *testing.T) {
	table := mustTable(t, 2, testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000))
	engine := mustEngine(t, table, 1_000, 5, identityQuoter())
	if err := engine.SetK(9); err != nil {
		t.Fatalf("SetK(9): %v", err)
	}
	priced := engine.WithGas(GasConfig{Units: 1})
	if got := priced.K(); got != 9 {
		t.Fatalf("copy K() = %d, want 9", got)
	}
	if err := engine.SetK(11); err != nil {
		t.Fatalf("SetK(11): %v", err)
	}
	if got := priced.K(); got != 9 {
		t.Fatalf("copy K() = %d after the original changed, want 9", got)
	}
}

// TestEngineSetKConcurrentWithEvaluate is a race-detector smoke test: the cap
// must be addressable as an atomic while evaluations run.
func TestEngineSetKConcurrentWithEvaluate(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x01, 0x02, 1_000_000, 2_000_000)
	table := mustTable(t, 2, poolA, poolB)
	view := newFakeView().add(poolA).add(poolB)
	engine := mustEngine(t, table, 1_000, 1, identityQuoter())
	dirty := []domain.PoolKey{poolA.Key}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 100; i++ {
			_ = engine.SetK(i)
		}
	}()
	for i := 0; i < 100; i++ {
		if _, err := engine.Evaluate(context.Background(), view, dirty); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
	}
	<-done
}
