package ingest

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestCamelotReceiptFeeSpacingAndAtomicRollback(t *testing.T) {
	p := v3Pool(domain.Address{1})
	p.Key.Kind = domain.PoolKindCamelotV3
	store := storeWith(t, p)
	if err := store.SetDeep(p.Key, pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}}); err != nil {
		t.Fatal(err)
	}
	branch, err := NewTransactionBranch(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.Begin(domain.BlockRef{Number: 1, Hash: domain.Hash{1}}); err != nil {
		t.Fatal(err)
	}
	fee := Log{Address: p.Key.Address, Topics: []domain.Hash{camelotv3.TopicFee}, Data: encodeWords(big.NewInt(0), big.NewInt(3000))}
	bad := Log{Address: p.Key.Address, Topics: []domain.Hash{camelotv3.TopicTickSpacing}, Data: encodeWords(big.NewInt(-1))}
	tx := Transaction{Hash: domain.Hash{2}, Successful: true, Logs: []Log{fee, bad}}
	if _, err := branch.Apply(t.Context(), tx); err == nil {
		t.Fatal("malformed metadata accepted")
	}
	unchanged, _ := branch.Working().Pool(p.Key)
	if unchanged.FeeOneForZero != nil {
		t.Fatal("failed transaction leaked fee change")
	}
	tx.Logs[1].Data = encodeWords(big.NewInt(60))
	if _, err := branch.Apply(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	updated, _ := branch.Working().Pool(p.Key)
	if updated.Fee != 0 || updated.FeeOneForZero == nil || *updated.FeeOneForZero != 3000 || updated.TickSpacing != 60 {
		t.Fatal("full receipt metadata missing")
	}
	if _, ok := branch.Working().Deep(p.Key); ok {
		t.Fatal("spacing change kept old tick bitmap")
	}
}
