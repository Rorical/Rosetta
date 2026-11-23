package backtest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestNewValidation(t *testing.T) {
	table := mustTable(t, testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000))
	notional := fixedNotional(1_000)
	quoter := v2Quoter()
	tests := []struct {
		name     string
		table    *graph.Table
		quoter   arb.Quoter
		notional arb.NotionalFunc
		k        int
		wantErr  bool
	}{
		{name: "nil table", table: nil, quoter: quoter, notional: notional, k: 1, wantErr: true},
		{name: "nil quoter", table: table, quoter: nil, notional: notional, k: 1, wantErr: true},
		{name: "nil notional", table: table, quoter: quoter, notional: nil, k: 1, wantErr: true},
		{name: "zero k", table: table, quoter: quoter, notional: notional, k: 0, wantErr: true},
		{name: "negative k", table: table, quoter: quoter, notional: notional, k: -1, wantErr: true},
		{name: "valid", table: table, quoter: quoter, notional: notional, k: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := New(test.table, test.quoter, test.notional, test.k)
			if test.wantErr {
				if !errors.Is(err, arb.ErrInvalidConfig) {
					t.Fatalf("error = %v, want %v", err, arb.ErrInvalidConfig)
				}
				if runner != nil {
					t.Fatal("runner = non-nil, want nil on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if runner == nil {
				t.Fatal("runner = nil, want non-nil")
			}
		})
	}
}

func TestRunReplaysBlocks(t *testing.T) {
	poolA := testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2Pool(0x20, 0x02, 0x01, 1_000_000, 1_000_000)
	runner, _ := mustRunner(t, poolA, poolB)

	blocks := []Block{
		testBlock(100, 1_000, upsertPools(poolA, poolB)),
		testBlock(101, 1_001, upsertPools(testV2Pool(0x10, 0x01, 0x02, 1_000_000, 1_000_000))),
	}
	steps, err := runner.Run(context.Background(), blocks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(steps) != len(blocks) {
		t.Fatalf("steps = %d, want %d", len(steps), len(blocks))
	}
	for i, step := range steps {
		if step.Ref != blocks[i].Ref {
			t.Fatalf("step %d ref = %+v, want %+v", i, step.Ref, blocks[i].Ref)
		}
	}

	if len(steps[0].Opportunities) != 2 {
		t.Fatalf("block 100 opportunities = %d, want 2", len(steps[0].Opportunities))
	}
	requireNet(t, steps[0], 0, 57190)
	requireNet(t, steps[0], 1, -501)
	if got := steps[0].Opportunities[0].Cycle.Pools[0].Address; got != poolA.Key.Address {
		t.Fatalf("block 100 best cycle starts at pool %s, want %s", got, poolA.Key.Address)
	}

	if len(steps[1].Opportunities) != 2 {
		t.Fatalf("block 101 opportunities = %d, want 2", len(steps[1].Opportunities))
	}
	requireNet(t, steps[1], 0, -2)
	requireNet(t, steps[1], 1, -2)
	if got := steps[1].Opportunities[0].Cycle.Pools[0].Address; got != poolA.Key.Address {
		t.Fatalf("block 101 best cycle starts at pool %s, want %s", got, poolA.Key.Address)
	}
}

func TestRunClearsDirtyBetweenBlocks(t *testing.T) {
	poolA := testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2Pool(0x20, 0x02, 0x01, 1_000_000, 1_000_000)
	runner, quoter := mustRunner(t, poolA, poolB)

	blocks := []Block{
		testBlock(100, 1_000, upsertPools(poolA, poolB)),
		testBlock(101, 1_001, nil),
	}
	steps, err := runner.Run(context.Background(), blocks)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(steps[0].Opportunities) != 2 {
		t.Fatalf("block 100 opportunities = %d, want 2", len(steps[0].Opportunities))
	}
	if len(steps[1].Opportunities) != 0 {
		t.Fatalf("block 101 opportunities = %d, want 0; dirty set leaked", len(steps[1].Opportunities))
	}
	if quoter.calls != 6 {
		t.Fatalf("quote calls = %d, want 6; block 101 must not be quoted", quoter.calls)
	}
}

func TestRunNilApply(t *testing.T) {
	poolA := testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2Pool(0x20, 0x02, 0x01, 1_000_000, 1_000_000)
	runner, quoter := mustRunner(t, poolA, poolB)

	steps, err := runner.Run(context.Background(), []Block{testBlock(100, 1_000, nil)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	if len(steps[0].Opportunities) != 0 {
		t.Fatalf("opportunities = %d, want 0", len(steps[0].Opportunities))
	}
	if quoter.calls != 0 {
		t.Fatalf("quote calls = %d, want 0", quoter.calls)
	}
}

func TestRunApplyError(t *testing.T) {
	poolA := testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2Pool(0x20, 0x02, 0x01, 1_000_000, 1_000_000)
	runner, _ := mustRunner(t, poolA, poolB)
	errApply := errors.New("malformed log")

	blocks := []Block{
		testBlock(100, 1_000, upsertPools(poolA, poolB)),
		testBlock(101, 1_001, func(*state.Store) error { return errApply }),
	}
	steps, err := runner.Run(context.Background(), blocks)
	if !errors.Is(err, errApply) {
		t.Fatalf("error = %v, want %v", err, errApply)
	}
	if !strings.Contains(err.Error(), "101") {
		t.Fatalf("error = %q, want block 101", err)
	}
	if len(steps) != 1 || steps[0].Ref != blocks[0].Ref {
		t.Fatalf("steps = %+v, want only block 100", steps)
	}
}

func TestRunBeginBlockError(t *testing.T) {
	poolA := testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	runner, _ := mustRunner(t, poolA)

	blocks := []Block{
		testBlock(100, 1_000, upsertPools(poolA)),
		testBlock(99, 1_001, nil),
	}
	steps, err := runner.Run(context.Background(), blocks)
	if !errors.Is(err, state.ErrBlockOutOfOrder) {
		t.Fatalf("error = %v, want %v", err, state.ErrBlockOutOfOrder)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("error = %q, want block 99", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
}

func TestRunFreshStorePerRun(t *testing.T) {
	poolA := testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2Pool(0x20, 0x02, 0x01, 1_000_000, 1_000_000)
	runner, _ := mustRunner(t, poolA, poolB)

	first, err := runner.Run(context.Background(), []Block{testBlock(100, 1_000, upsertPools(poolA, poolB))})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(first[0].Opportunities) != 2 {
		t.Fatalf("first run opportunities = %d, want 2", len(first[0].Opportunities))
	}

	// Only poolA is written. A cycle needs both pools, so any opportunity here
	// means poolB survived the first run.
	second, err := runner.Run(context.Background(), []Block{testBlock(200, 2_000, upsertPools(poolA))})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("second run steps = %d, want 1", len(second))
	}
	if len(second[0].Opportunities) != 0 {
		t.Fatalf("second run opportunities = %d, want 0; state leaked between runs", len(second[0].Opportunities))
	}
}

func TestRunEmptyBlocks(t *testing.T) {
	runner, _ := mustRunner(t, testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000))

	steps, err := runner.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if steps == nil {
		t.Fatal("steps = nil, want empty slice")
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %d, want 0", len(steps))
	}
}

func TestRunCancelledBetweenBlocks(t *testing.T) {
	poolA := testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2Pool(0x20, 0x02, 0x01, 1_000_000, 1_000_000)
	runner, quoter := mustRunner(t, poolA, poolB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Block 100 quotes both cycles at the notional and refines the profitable
	// one at its closed-form size (six legs); cancel after the last one so the
	// block still completes and the next block observes the cancellation.
	quoter.onCall = func(call int) {
		if call == 6 {
			cancel()
		}
	}

	blocks := []Block{
		testBlock(100, 1_000, upsertPools(poolA, poolB)),
		testBlock(101, 1_001, upsertPools(poolA)),
	}
	steps, err := runner.Run(ctx, blocks)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
	if len(steps) != 1 || steps[0].Ref != blocks[0].Ref {
		t.Fatalf("steps = %+v, want only block 100", steps)
	}
}

func TestRunCancelledDuringEvaluation(t *testing.T) {
	poolA := testV2Pool(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2Pool(0x20, 0x02, 0x01, 1_000_000, 1_000_000)
	runner, quoter := mustRunner(t, poolA, poolB)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel during the first cycle's last leg. Evaluate sees the cancellation
	// before the next candidate, so the block is not recorded.
	quoter.onCall = func(call int) {
		if call == 2 {
			cancel()
		}
	}

	steps, err := runner.Run(ctx, []Block{testBlock(100, 1_000, upsertPools(poolA, poolB))})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
	if !strings.Contains(err.Error(), "100") {
		t.Fatalf("error = %q, want block 100", err)
	}
	if len(steps) != 0 {
		t.Fatalf("steps = %d, want 0", len(steps))
	}
}
