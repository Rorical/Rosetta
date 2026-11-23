package exec

import (
	"errors"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func improvedIntent(t *testing.T, i ReactiveIntent) ReactiveIntent {
	t.Helper()
	i.Route.Steps = append([]ExecutorStep(nil), i.Route.Steps...)
	i.Route.Amount = big.NewInt(90)
	i.Route.Steps[0].Pool, i.Route.Steps[1].Pool = i.Route.Steps[1].Pool, i.Route.Steps[0].Pool
	i.Route.MinProfit = big.NewInt(110001)
	i.Call.FeeCap, i.Call.TipCap = big.NewInt(3), big.NewInt(2)
	i.Budget, i.QuotedGross, i.ExpectedNet = big.NewInt(110000), big.NewInt(110020), big.NewInt(20)
	var err error
	i.Call, err = BuildExecutorCall(t.Context(), i.Route, i.Call.CallParameters)
	if err != nil {
		t.Fatal(err)
	}
	i, err = SealReactiveIntent(t.Context(), i)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestReplaceEconomicActionAndStopAfterObservedInclusion(t *testing.T) {
	i, l, s := dispatchFixture(t)
	l.PerDay, l.PerTransaction = big.NewInt(300000), big.NewInt(200000)
	n := &dispatchNetworkFixture{}
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "state.json"), l, s, n)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	original, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	better := improvedIntent(t, i)
	if _, err = d.Replace(t.Context(), better, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	d.ObserveIncluded([]domain.Hash{original})
	if !d.Included() {
		t.Fatal("original variant inclusion missed")
	}
	if _, err = d.Replace(t.Context(), better, func() bool { return true }); !errors.Is(err, ErrDispatchRejected) {
		t.Fatal(err)
	}
	if n.calls != 2 {
		t.Fatal("sent after inclusion")
	}
}

func TestCancellationPersistsOriginalAndSettlesWithoutExecutorEvent(t *testing.T) {
	i, l, s := dispatchFixture(t)
	l.PerDay = big.NewInt(300000)
	n := &dispatchNetworkFixture{}
	path := filepath.Join(t.TempDir(), "state.json")
	d, err := OpenDispatcher(t.Context(), path, l, s, n)
	if err != nil {
		t.Fatal(err)
	}
	original, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	c := Cancellation{EstimatedFailureFee: big.NewInt(100000), Target: original, FeeCap: big.NewInt(3), TipCap: big.NewInt(2), GasLimit: 21000, Budget: big.NewInt(63000), CreatedMS: now, ExpiresMS: now + 500, Reason: "remaining_profit_below_guard"}
	hash, err := d.Cancel(t.Context(), c, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if d.store.state.Charged.Int64() != 100000 {
		t.Fatal("same nonce cancellation double charged")
	}
	d.Close()
	d, err = OpenDispatcher(t.Context(), path, l, s, n)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	p := d.Pending()
	if p.Cancellation == nil || len(p.Previous) != 1 || p.Previous[0].Hash != original {
		t.Fatal("lost original across restart")
	}
	receipt, err := settleExecutor(*p, &rpc.TransactionReceipt{Hash: hash, Success: true, Fees: &domain.ReceiptFees{Total: big.NewInt(22)}})
	if err != nil || !receipt.Cancelled || receipt.Net.Int64() != -22 {
		t.Fatal(receipt, err)
	}
	d.ObserveCanonical(domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, []CanonicalReceipt{{Hash: hash, Success: true, Fees: receipt.Fees}})
	updates := followAll(t, d, time.Now())
	if len(updates) != 1 || !updates[0].Settlement.Cancelled || updates[0].Settlement.Net.Int64() != -22 {
		t.Fatal("cancellation not booked", updates)
	}
	if d.Busy() || d.store.state.NextNonce != 1 {
		t.Fatal("cancellation did not release nonce")
	}
}

func TestStaleInclusionNotificationCannotBlockNextNonce(t *testing.T) {
	d := &Dispatcher{}
	first := &PendingTransaction{Nonce: 0, Hash: domain.Hash{1}}
	d.pending.Store(first)
	d.ObserveIncluded([]domain.Hash{first.Hash})
	if !d.Included() {
		t.Fatal("inclusion lost")
	}
	d.pending.Store(&PendingTransaction{Nonce: 1, Hash: domain.Hash{2}})
	d.observeNonce(first.Nonce)
	if d.Included() {
		t.Fatal("stale observer blocked next nonce")
	}
	d.ObserveIncluded([]domain.Hash{{2}})
	d.observeNonce(first.Nonce)
	if !d.Included() {
		t.Fatal("stale observer erased newer inclusion")
	}
}
