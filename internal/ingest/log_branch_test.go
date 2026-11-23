package ingest

import (
	"errors"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

func logMintBranch(t *testing.T) (*LogBranch, *TransactionBranch, domain.PoolKey) {
	t.Helper()
	address := domain.Address{1}
	base := seededV3Pool(t, address)
	committedAt(t, base, domain.BlockRef{Number: 10, Hash: domain.Hash{10}})
	branch, err := NewLogBranch(base)
	if err != nil {
		t.Fatal(err)
	}
	standard, err := NewTransactionBranch(base)
	if err != nil {
		t.Fatal(err)
	}
	return branch, standard, v3Key(address)
}

func logMintTx(key domain.PoolKey, id byte, index uint64, amount int64) Transaction {
	return Transaction{Hash: domain.Hash{id}, Index: index, Successful: true, Logs: []Log{v3MintLog(key.Address, -120, 120, amount)}}
}

func commitLogBlock(t *testing.T, standard *TransactionBranch, number uint64, txs []Transaction) *state.Store {
	t.Helper()
	ref := domain.BlockRef{Number: number, Hash: domain.Hash{byte(number)}, ParentHash: standard.Base().Block().Hash}
	if err := standard.Begin(ref); err != nil {
		t.Fatal(err)
	}
	hashes := make([]domain.Hash, len(txs))
	for i, tx := range txs {
		if _, err := standard.Apply(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
		hashes[i] = tx.Hash
	}
	if err := standard.Commit(ref, hashes); err != nil {
		t.Fatal(err)
	}
	return standard.Base()
}

func TestLogBranchRebasesAdditiveEffectsAndRepairsMissingTail(t *testing.T) {
	for _, missingTail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "missing tail"}[missingTail], func(t *testing.T) {
			branch, standard, key := logMintBranch(t)
			first := logMintTx(key, 1, 0, 100)
			for number := uint64(11); number <= 13; number++ {
				if err := branch.Begin(t.Context(), domain.BlockRef{Number: number, ParentHash: domain.Hash{byte(number - 1)}}); err != nil {
					t.Fatal(err)
				}
				tx := logMintTx(key, byte(number-10), 0, int64(number-10)*100)
				if _, err := branch.Apply(t.Context(), tx); err != nil {
					t.Fatal(err)
				}
			}
			oldView, oldVersion, _ := branch.View()
			p, _ := oldView.Pool(key)
			assertInt(t, "speculative liquidity", p.Liquidity, 1100)
			if oldView.Block().Hash != (domain.Hash{}) {
				t.Fatal("tentative block used a verified hash")
			}
			txs := []Transaction{first}
			want := int64(1100)
			if missingTail {
				txs = append(txs, logMintTx(key, 9, 1, 50))
				want += 50
			}
			base := commitLogBlock(t, standard, 11, txs)
			matched, err := branch.Reconcile(t.Context(), base, txs)
			if err != nil || matched == missingTail {
				t.Fatalf("reconcile matched=%v error=%v", matched, err)
			}
			view, version, ok := branch.View()
			p, _ = view.Pool(key)
			if !ok || version.BlockNumber != 13 || version.Anchor != base.Block() || version.Epoch == oldVersion.Epoch {
				t.Fatal("rebase lost suffix or retained old ticket")
			}
			assertInt(t, "rebased liquidity", p.Liquidity, want)
			p, _ = oldView.Pool(key)
			assertInt(t, "old snapshot liquidity", p.Liquidity, 1100)
			p, _ = base.Pool(key)
			assertInt(t, "canonical liquidity", p.Liquidity, want-500)
		})
	}
}

func TestLogBranchEmptyBlocksAndOwnedReceiptReplay(t *testing.T) {
	branch, standard, key := logMintBranch(t)
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: 11, ParentHash: domain.Hash{10}}); err != nil {
		t.Fatal(err)
	}
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: 12, ParentHash: domain.Hash{11}}); err != nil {
		t.Fatal(err)
	}
	tx := logMintTx(key, 1, 0, 100)
	if _, err := branch.Apply(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	tx.Logs[0].Data[0] ^= 255
	tx.Logs[0].Topics[0] = domain.Hash{}
	base := commitLogBlock(t, standard, 11, nil)
	if matched, err := branch.Reconcile(t.Context(), base, nil); err != nil || !matched {
		t.Fatalf("empty block reconcile: %v %v", matched, err)
	}
	if err := branch.RefreshBase(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	view, version, ok := branch.View()
	p, _ := view.Pool(key)
	if !ok || version.BlockNumber != 12 {
		t.Fatal("empty predecessor discarded current block")
	}
	assertInt(t, "owned receipt liquidity", p.Liquidity, 600)
}

func TestLogBranchForkAndPayloadRewind(t *testing.T) {
	branch, standard, key := logMintBranch(t)
	for number := uint64(11); number <= 12; number++ {
		if err := branch.Begin(t.Context(), domain.BlockRef{Number: number, ParentHash: domain.Hash{byte(number - 1)}}); err != nil {
			t.Fatal(err)
		}
		if _, err := branch.Apply(t.Context(), logMintTx(key, byte(number), 0, 100)); err != nil {
			t.Fatal(err)
		}
	}
	if err := branch.Rewind(t.Context(), 12); err != nil {
		t.Fatal(err)
	}
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: 12, ParentHash: domain.Hash{99}}); !errors.Is(err, ErrFastContinuity) {
		t.Fatal("replacement silently changed predecessor seal", err)
	}
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: 12, ParentHash: domain.Hash{11}}); err != nil {
		t.Fatal(err)
	}
	tx := logMintTx(key, 11, 0, 100)
	base := commitLogBlock(t, standard, 11, []Transaction{tx})
	fork := base.View().Fork()
	ref := base.Block()
	ref.Hash = domain.Hash{99}
	if err := fork.BeginBlock(ref); err != nil {
		t.Fatal(err)
	}
	fork.Commit()
	if _, err := branch.Reconcile(t.Context(), fork, []Transaction{tx}); !errors.Is(err, ErrFastConflict) {
		t.Fatal("detached feed suffix survived", err)
	}
	if _, _, ok := branch.View(); ok {
		t.Fatal("detached state remains available")
	}
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: 12, ParentHash: ref.Hash}); err != nil {
		t.Fatal("could not attach to repaired base", err)
	}
}

func TestLogBranchBoundsEmptyBlockHistory(t *testing.T) {
	branch, _, _ := logMintBranch(t)
	for i := 0; i < maxLogBlocks; i++ {
		number := uint64(i + 11)
		if err := branch.Begin(t.Context(), domain.BlockRef{Number: number, ParentHash: domain.Hash{byte(number - 1)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := branch.Begin(t.Context(), domain.BlockRef{Number: uint64(11 + maxLogBlocks), ParentHash: domain.Hash{byte(10 + maxLogBlocks)}}); !errors.Is(err, ErrFastLimit) {
		t.Fatal("unbounded empty block cache", err)
	}
}
