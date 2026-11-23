package ingest

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func fastBranch(t *testing.T, opts FastOptions) (*FastBranch, *TransactionBranch, domain.BlockRef, domain.PoolKey) {
	t.Helper()
	standard, ref, key := transactionBranch(t)
	fast, err := NewFastBranch(standard.Base(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return fast, standard, ref, key
}

func fastSync(key domain.PoolKey, block, index uint64, id byte, reserve int64) feed.FastMessage {
	log := syncLog(key, reserve, reserve+100)
	return feed.FastMessage{
		BlockNumber: block, TxIndex: index, TxHash: domain.Hash{id}, RawTx: []byte{id},
		Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{{Address: log.Address, Topics: log.Topics, Data: log.Data}}},
	}
}

func TestFastBranchAtomicViewsVersionsAndPrefixOwnership(t *testing.T) {
	fast, standard, _, key := fastBranch(t, FastOptions{})
	first := fastSync(key, 11, 0, 1, 300)
	dirty, err := fast.Apply(context.Background(), first)
	if err != nil || len(dirty) != 1 || dirty[0] != key {
		t.Fatalf("apply dirty=%v error=%v", dirty, err)
	}
	view, version, revisions, valid := fast.View()
	if !valid || view.Block().Hash != (domain.Hash{}) || version.Anchor != standard.Base().Block() || revisions[key] != 1 {
		t.Fatal("tentative version lost its anchor or masqueraded as a verified block")
	}
	accepted, _ := standard.Base().Pool(key)
	assertInt(t, "canonical reserve", accepted.Reserve0, 1000)
	if _, err := fast.Apply(context.Background(), first); err != nil {
		t.Fatalf("identical duplicate: %v", err)
	}
	if !fast.Current(version) {
		t.Fatal("duplicate advanced prefix")
	}
	if len(fast.records) != 1 {
		t.Fatal("missing prefix record")
	}
	first.RawTx[0] = 0xee
	first.Receipt.Logs[0].Topics[0] = domain.Hash{}
	if owned := fast.records[0].message; owned.RawTx[0] != 1 || owned.Receipt.Logs[0].Topics[0] == (domain.Hash{}) {
		t.Fatal("caller mutated prefix record")
	}
	if _, err := fast.Apply(context.Background(), fastSync(key, 11, 1, 2, 500)); err != nil {
		t.Fatal(err)
	}
	if fast.Current(version) {
		t.Fatal("new transaction left old version current")
	}
	old, _ := view.Pool(key)
	assertInt(t, "immutable old view", old.Reserve0, 300)
	fast.Reset()
	if _, _, _, ok := fast.View(); ok {
		t.Fatal("reset retained a valid Fast view")
	}
}

func TestFastBranchRejectsMissingPrefixConflictsAndLimits(t *testing.T) {
	tests := []struct {
		name string
		opts FastOptions
		next func(domain.PoolKey) feed.FastMessage
		want error
	}{
		{"missing transaction", FastOptions{}, func(k domain.PoolKey) feed.FastMessage { return fastSync(k, 11, 2, 2, 500) }, ErrFastContinuity},
		{"missing block", FastOptions{}, func(k domain.PoolKey) feed.FastMessage { return fastSync(k, 13, 0, 2, 500) }, ErrFastContinuity},
		{"changed receipt", FastOptions{}, func(k domain.PoolKey) feed.FastMessage { return fastSync(k, 11, 0, 1, 500) }, ErrFastConflict},
		{"changed hash", FastOptions{}, func(k domain.PoolKey) feed.FastMessage { return fastSync(k, 11, 0, 2, 300) }, ErrFastConflict},
		{"transaction limit", FastOptions{MaxTransactions: 1}, func(k domain.PoolKey) feed.FastMessage { return fastSync(k, 11, 1, 2, 500) }, ErrFastLimit},
		{"byte limit", FastOptions{MaxBytes: 500}, func(k domain.PoolKey) feed.FastMessage { return fastSync(k, 11, 1, 2, 500) }, ErrFastLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fast, _, _, key := fastBranch(t, test.opts)
			if _, err := fast.Apply(context.Background(), fastSync(key, 11, 0, 1, 300)); err != nil {
				t.Fatal(err)
			}
			_, version, _, _ := fast.View()
			if _, err := fast.Apply(context.Background(), test.next(key)); !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
			if fast.Current(version) {
				t.Fatal("invalid prefix remained current")
			}
			if _, err := fast.Apply(context.Background(), fastSync(key, 11, 1, 2, 500)); !errors.Is(err, ErrFastContinuity) {
				t.Fatal("rejoined an incomplete prefix")
			}
		})
	}
}

func TestFastBranchFailedReceiptAndMalformedLaterLog(t *testing.T) {
	fast, _, _, key := fastBranch(t, FastOptions{FirstTxIndex: 1, CanonicalFirstTxIndex: 1})
	message := fastSync(key, 11, 1, 1, 300)
	message.Receipt = feed.FastReceipt{}
	if dirty, err := fast.Apply(context.Background(), message); err != nil || len(dirty) != 0 {
		t.Fatalf("reverted receipt: %v %v", dirty, err)
	}
	_, version, _, _ := fast.View()
	message = fastSync(key, 11, 2, 2, 500)
	message.Receipt.Logs = append(message.Receipt.Logs, feed.FastLog{Address: key.Address, Topics: message.Receipt.Logs[0].Topics, Data: []byte{1}})
	if _, err := fast.Apply(context.Background(), message); err == nil || fast.Current(version) {
		t.Fatal("malformed complete receipt retained a usable branch")
	}
}

func confirmFastBlock(t *testing.T, standard *TransactionBranch, ref domain.BlockRef, messages ...feed.FastMessage) []Transaction {
	t.Helper()
	if err := standard.Begin(ref); err != nil {
		t.Fatal(err)
	}
	txs := make([]Transaction, 0, len(messages)+1)
	txs = append(txs, Transaction{Hash: domain.Hash{0x6a}, Successful: true})
	for _, message := range messages {
		tx := fastTransaction(message)
		tx.Index = uint64(len(txs))
		txs = append(txs, tx)
	}
	hashes := make([]domain.Hash, len(txs))
	for i, tx := range txs {
		if _, err := standard.Apply(context.Background(), tx); err != nil {
			t.Fatal(err)
		}
		hashes[i] = tx.Hash
	}
	if err := standard.Commit(ref, hashes); err != nil {
		t.Fatal(err)
	}
	return txs
}

func TestFastBranchReconcileDropsConfirmedPrefixAndRebasesFutureBlock(t *testing.T) {
	fast, standard, ref, key := fastBranch(t, FastOptions{CanonicalFirstTxIndex: 1})
	first, future := fastSync(key, 11, 0, 1, 300), fastSync(key, 12, 0, 2, 500)
	for _, message := range []feed.FastMessage{first, future} {
		if _, err := fast.Apply(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	_, old, _, _ := fast.View()
	txs := confirmFastBlock(t, standard, ref, first)
	affected, err := fast.Reconcile(context.Background(), standard.Base(), txs)
	if err != nil {
		t.Fatal(err)
	}
	if len(affected) != 1 || affected[0] != key {
		t.Fatal("replayed future prefix did not report its pool", affected)
	}
	view, version, revisions, ok := fast.View()
	if !ok || fast.Current(old) || version.Epoch == old.Epoch || version.Anchor != ref || revisions[key] != 1 {
		t.Fatal("rebase reused stale version or confirmed changes")
	}
	if len(fast.records) != 1 || fast.records[0].message.BlockNumber != 12 {
		t.Fatal("confirmed raw tx retained or future prefix lost")
	}
	current, _ := view.Pool(key)
	assertInt(t, "rebased future reserve", current.Reserve0, 500)
	canonical, _ := standard.Base().Pool(key)
	assertInt(t, "standard reserve", canonical.Reserve0, 300)
}

func TestFastBranchCanonicalConflictAndReorgKeepOnlyVerifiedBase(t *testing.T) {
	for _, reorg := range []bool{false, true} {
		fast, standard, ref, key := fastBranch(t, FastOptions{CanonicalFirstTxIndex: 1})
		if _, err := fast.Apply(context.Background(), fastSync(key, 11, 0, 1, 300)); err != nil {
			t.Fatal(err)
		}
		txs := confirmFastBlock(t, standard, ref, fastSync(key, 11, 0, 1, 600))
		base := standard.Base()
		want := ErrFastConflict
		if reorg {
			copy := state.New()
			if err := copy.Restore(base.Checkpoint()); err != nil {
				t.Fatal(err)
			}
			ref.ParentHash = domain.Hash{0xff}
			if err := copy.BeginBlock(ref); err != nil {
				t.Fatal(err)
			}
			copy.Commit()
			base, want = copy, ErrFastContinuity
		}
		if affected, err := fast.Reconcile(context.Background(), base, txs); !errors.Is(err, want) || affected != nil {
			t.Fatalf("got %v want %v", err, want)
		}
		if _, _, _, ok := fast.View(); ok {
			t.Fatal("conflicting/reorg branch retained")
		}
		if _, err := fast.Apply(context.Background(), fastSync(key, 12, 0, 2, 700)); err != nil {
			t.Fatalf("failed to recover from verified base: %v", err)
		}
	}
}

// A reset branch whose base trails the stream by a block waits with the next
// block's run instead of refusing it, and resumes when the base reaches the
// run's parent.
func TestFastBranchWaitsAheadOfItsBaseAndResumesOnReconcile(t *testing.T) {
	fast, standard, ref, key := fastBranch(t, FastOptions{CanonicalFirstTxIndex: 1})
	if _, err := fast.Apply(context.Background(), fastSync(key, 12, 1, 9, 100)); !errors.Is(err, ErrFastContinuity) {
		t.Fatal("a mid-block message cannot start a run", err)
	}
	for i, message := range []feed.FastMessage{fastSync(key, 12, 0, 1, 300), fastSync(key, 12, 1, 2, 500)} {
		if dirty, err := fast.Apply(context.Background(), message); err != nil || dirty != nil {
			t.Fatal("message ahead of the base not kept", i, err)
		}
	}
	if _, _, _, ok := fast.View(); ok {
		t.Fatal("waiting messages applied before their parent")
	}
	txs := confirmFastBlock(t, standard, ref)
	affected, err := fast.Reconcile(context.Background(), standard.Base(), txs)
	if err != nil {
		t.Fatal(err)
	}
	view, version, _, ok := fast.View()
	if !ok || len(affected) != 1 || version.BlockNumber != 12 || version.TxIndex != 1 || version.Anchor != ref {
		t.Fatal("did not resume from the waiting run", version, affected)
	}
	current, _ := view.Pool(key)
	assertInt(t, "resumed reserve", current.Reserve0, 500)
}

// A Fast block has no header yet. Its time is the parent's, which ArbOS never
// goes below; a zero time read every Slipstream oracle as idle for the whole
// lookback, so quotes charged only the base fee (1308 pips quoted against
// 1471 charged, and the route reverted at its profit check).
func TestFastBlockKeepsTheParentTimeForSlipstreamFees(t *testing.T) {
	key := domain.PoolKey{Address: domain.Address{0x51}, Kind: domain.PoolKindSlipstream}
	slip := pool.State{Key: key, Token0: domain.Address{1}, Token1: domain.Address{2}, Tick: 20, Fee: 110, TickSpacing: 1, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1),
		Slipstream: &pool.SlipstreamState{Valid: true, Block: 10, Timestamp: 100, BaseFee: 100, SpacingFee: 500, DefaultFeeCap: 50000, DefaultScalingFactor: 1_000_000, SecondsAgo: 10, ObservationIndex: 1, Cardinality: 2, CardinalityNext: 2, Complete: true,
			Observations: pool.NewOracleHistory([]pool.OracleObservation{{Index: 0, Timestamp: 90, TickCumulative: 0}, {Index: 1, Timestamp: 100, TickCumulative: 100}})}}
	base := storeWith(t, slip)
	committedAt(t, base, domain.BlockRef{Number: 10, Hash: domain.Hash{10}, Timestamp: 100})
	fast, err := NewFastBranch(base, FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	message := feed.FastMessage{BlockNumber: 11, TxHash: domain.Hash{1}, RawTx: []byte{1}, Receipt: feed.FastReceipt{Successful: true}}
	if _, err := fast.Apply(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	view, _, _, ok := fast.View()
	if !ok {
		t.Fatal("no Fast view")
	}
	got, _ := view.Pool(key)
	// TWAP 10 against tick 20 at the parent's time: 100 + 10.
	if got.Fee != 110 || got.Slipstream.Timestamp != 100 || got.Slipstream.Block != 11 {
		t.Fatalf("fee %d at time %d block %d", got.Fee, got.Slipstream.Timestamp, got.Slipstream.Block)
	}
}
