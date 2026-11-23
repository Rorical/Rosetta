package ingest

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

func transactionBranch(t *testing.T) (*TransactionBranch, domain.BlockRef, domain.PoolKey) {
	t.Helper()
	p := v2Pool(domain.Address{0x11})
	base := storeWith(t, p)
	parent := domain.BlockRef{Number: 10, Hash: domain.Hash{0x10}}
	if err := base.BeginBlock(parent); err != nil {
		t.Fatal(err)
	}
	base.Commit()
	branch, err := NewTransactionBranch(base)
	if err != nil {
		t.Fatal(err)
	}
	return branch, domain.BlockRef{Number: 11, Hash: domain.Hash{0x11}, ParentHash: parent.Hash}, p.Key
}

func syncLog(key domain.PoolKey, reserve0, reserve1 int64) Log {
	return Log{Address: key.Address, Topics: []domain.Hash{uniswapv2.TopicSync}, Data: encodeWords(big.NewInt(reserve0), big.NewInt(reserve1))}
}

func TestTransactionBranchWholeTransactionAndBlockCommit(t *testing.T) {
	branch, ref, key := transactionBranch(t)
	original := branch.Base()
	if err := branch.Begin(ref); err != nil {
		t.Fatal(err)
	}
	tx := Transaction{Hash: domain.Hash{0x01}, Successful: true, Logs: []Log{syncLog(key, 300, 400), syncLog(key, 500, 600)}}
	dirty, err := branch.Apply(context.Background(), tx)
	if err != nil || len(dirty) != 1 || dirty[0] != key {
		t.Fatalf("dirty=%v error=%v", dirty, err)
	}
	working, _ := branch.Working().Pool(key)
	accepted, _ := branch.Base().Pool(key)
	assertInt(t, "working reserve", working.Reserve0, 500)
	assertInt(t, "accepted reserve", accepted.Reserve0, 1000)
	if branch.Commit(ref, nil) == nil || branch.Base() != original {
		t.Fatal("missing accepted receipt advanced the base")
	}
	wrong := ref
	wrong.Hash = domain.Hash{0xff}
	if branch.Commit(wrong, []domain.Hash{tx.Hash}) == nil {
		t.Fatal("mismatching block hash committed")
	}
	ref.Timestamp = 123
	ref.L1Block = 456
	if err := branch.Commit(ref, []domain.Hash{tx.Hash}); err != nil {
		t.Fatal(err)
	}
	accepted, _ = branch.Base().Pool(key)
	assertInt(t, "committed reserve", accepted.Reserve0, 500)
	if branch.Working() != nil || branch.Base().Block() != ref {
		t.Fatal("commit lost final block metadata or retained the tentative branch")
	}
	unchanged, _ := original.Pool(key)
	assertInt(t, "original snapshot reserve", unchanged.Reserve0, 1000)
}

func TestTransactionBranchRollsBackAllLogsAndCanRetry(t *testing.T) {
	branch, ref, key := transactionBranch(t)
	if err := branch.Begin(ref); err != nil {
		t.Fatal(err)
	}
	first := Transaction{Hash: domain.Hash{0x01}, Successful: true, Logs: []Log{syncLog(key, 300, 400)}}
	if _, err := branch.Apply(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := Transaction{Hash: domain.Hash{0x02}, Index: 1, Successful: true, Logs: []Log{
		syncLog(key, 500, 600), {Address: key.Address, Topics: []domain.Hash{uniswapv2.TopicSync}, Data: []byte{1}},
	}}
	if _, err := branch.Apply(context.Background(), second); err == nil {
		t.Fatal("malformed later log was accepted")
	}
	current, _ := branch.Working().Pool(key)
	assertInt(t, "prior transaction reserve", current.Reserve0, 300)
	if len(branch.Working().Dirty()) != 0 {
		t.Fatal("failed transaction leaked dirty pools")
	}
	second.Logs = second.Logs[:1]
	if _, err := branch.Apply(context.Background(), second); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := branch.Commit(ref, []domain.Hash{first.Hash, second.Hash}); err != nil {
		t.Fatal(err)
	}
}

func TestTransactionBranchFailureIndexAndCancellation(t *testing.T) {
	branch, ref, key := transactionBranch(t)
	if err := branch.Begin(ref); err != nil {
		t.Fatal(err)
	}
	if branch.Begin(ref) == nil {
		t.Fatal("unfinished attempt was replaced")
	}
	failed := Transaction{Hash: domain.Hash{0x01}, Logs: []Log{syncLog(key, 1, 2)}}
	if _, err := branch.Apply(context.Background(), failed); err == nil {
		t.Fatal("failed receipt with logs was accepted")
	}
	failed.Logs = nil
	if dirty, err := branch.Apply(context.Background(), failed); err != nil || len(dirty) != 0 {
		t.Fatalf("failed receipt should occupy its index without pool effects: %v %v", dirty, err)
	}
	if _, err := branch.Apply(context.Background(), failed); err == nil {
		t.Fatal("duplicate receipt was accepted")
	}
	next := Transaction{Hash: domain.Hash{0x02}, Index: 1, Successful: true, Logs: []Log{syncLog(key, 1, 2)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := branch.Apply(ctx, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := branch.Apply(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	accepted, _ := branch.Base().Pool(key)
	assertInt(t, "uncommitted attempt reserve", accepted.Reserve0, 1000)
}

func TestTransactionBranchRejectsGap(t *testing.T) {
	branch, ref, _ := transactionBranch(t)
	ref.Number++
	if !errors.Is(branch.Begin(ref), ErrReorg) {
		t.Fatal("block gap accepted")
	}
	if _, err := NewTransactionBranch((*state.Store)(nil)); err == nil {
		t.Fatal("nil base accepted")
	}
}
