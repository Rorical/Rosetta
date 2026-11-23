package ingest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestLogBatchPublishesOnlyNewSuffixAndPreservesViews(t *testing.T) {
	branch, _, key := logMintBranch(t)
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: 11, ParentHash: domain.Hash{10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := branch.ApplyBatch(t.Context(), []Transaction{logMintTx(key, 1, 0, 100)}); err != nil {
		t.Fatal(err)
	}
	oldView, oldVersion, _ := branch.View()
	dirty, err := branch.ApplyBatch(t.Context(), []Transaction{logMintTx(key, 2, 1, 200), logMintTx(key, 3, 2, 300)})
	if err != nil {
		t.Fatal(err)
	}
	view, version, _ := branch.View()
	current, _ := view.Pool(key)
	prior, _ := oldView.Pool(key)
	assertInt(t, "new suffix applied once", current.Liquidity, 1100)
	assertInt(t, "old snapshot", prior.Liquidity, 600)
	if version.TxIndex != 2 || version == oldVersion || len(branch.blocks[0].transactions) != 3 || !reflect.DeepEqual(dirty, []domain.PoolKey{key}) {
		t.Fatal("batch did not publish one complete suffix")
	}
}

func TestLogBatchFailureLeavesEveryBranchFieldUnchanged(t *testing.T) {
	branch, _, key := logMintBranch(t)
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: 11, ParentHash: domain.Hash{10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := branch.ApplyBatch(t.Context(), []Transaction{logMintTx(key, 1, 0, 100)}); err != nil {
		t.Fatal(err)
	}
	working, version, size, count := branch.working, branch.version, branch.bytes, branch.transactions
	before, _, _ := branch.View()
	if _, err := branch.ApplyBatch(t.Context(), []Transaction{logMintTx(key, 2, 1, 200), logMintTx(key, 3, 9, 300)}); !errors.Is(err, ErrFastContinuity) {
		t.Fatal("invalid suffix accepted", err)
	}
	if branch.working != working || branch.version != version || branch.bytes != size || branch.transactions != count || len(branch.blocks[0].transactions) != 1 || len(branch.affected) != 1 {
		t.Fatal("failed suffix mutated branch metadata")
	}
	current, _ := branch.working.Pool(key)
	prior, _ := before.Pool(key)
	assertInt(t, "failed batch liquidity", current.Liquidity, 600)
	assertInt(t, "retained view liquidity", prior.Liquidity, 600)
	if _, err := branch.ApplyBatch(t.Context(), []Transaction{logMintTx(key, 2, 1, 200)}); err != nil {
		t.Fatal("failed batch poisoned next suffix", err)
	}
}

func TestLogBatchEmptyAndCancelledDoNotAdvance(t *testing.T) {
	branch, _, key := logMintBranch(t)
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: 11, ParentHash: domain.Hash{10}}); err != nil {
		t.Fatal(err)
	}
	version, working := branch.version, branch.working
	if dirty, err := branch.ApplyBatch(t.Context(), nil); err != nil || len(dirty) != 0 {
		t.Fatal(dirty, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := branch.ApplyBatch(ctx, []Transaction{logMintTx(key, 1, 0, 100)}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if branch.version != version || branch.working != working || branch.transactions != 0 {
		t.Fatal("empty or cancelled batch changed branch")
	}
}
