package exec

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func openSettlementFixture(t *testing.T) (*Dispatcher, ReactiveIntent, *dispatchNetworkFixture) {
	t.Helper()
	i, l, s := dispatchFixture(t)
	l.PerTransaction = big.NewInt(200000) // room for a replacement
	l.PerDay = new(big.Int).Mul(l.PerTransaction, big.NewInt(10))
	network := &dispatchNetworkFixture{}
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "state.json"), l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	quickFollowUps(d)
	return d, i, network
}

func settleFromFeed(t *testing.T, d *Dispatcher, hash domain.Hash, success bool, logs []rpc.Log) []SettlementUpdate {
	t.Helper()
	observed := d.ObserveExecution(hash, success, logs, ExecutionPosition{Stream: "flashblocks", BlockNumber: 10, FrameIndex: 2, ReceivedAtNS: time.Now().UnixNano()})
	if observed == nil {
		t.Fatal("own transaction not matched")
	}
	updates, err := d.SettleObserved(observed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return updates
}

// The feed books the outcome at once and opens the lane; the canonical block
// that follows supplies the paid fees without any RPC.
func TestFeedSettlesSuccessAndRevertBeforeFees(t *testing.T) {
	for _, success := range []bool{true, false} {
		d, i, network := openSettlementFixture(t)
		hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		var logs []rpc.Log
		if success {
			logs = observationLogs(i)
		}
		updates := settleFromFeed(t, d, hash, success, logs)
		if len(updates) != 1 || !updates[0].First || updates[0].FeesBooked {
			t.Fatal(updates)
		}
		booked := updates[0].Settlement
		wantProfit := new(big.Int)
		if success {
			wantProfit = i.QuotedGross
		}
		if booked.Hash != hash || booked.Success != success || booked.Profit.Cmp(wantProfit) != 0 || booked.Net != nil || booked.FeeStatus != FeesPending || booked.Intent == nil || booked.Position == nil {
			t.Fatalf("feed settlement %+v", booked)
		}
		if d.Busy() || d.store.state.NextNonce != 1 || d.store.state.Charged.Cmp(i.Budget) != 0 || len(d.store.state.Included) != 1 {
			t.Fatal("feed settlement did not open the lane or lost the reservation")
		}

		receipt := CanonicalReceipt{Hash: hash, Success: success, Logs: logs, Fees: &domain.ReceiptFees{GasUsed: 15000, Total: big.NewInt(300)}}
		d.ObserveCanonical(domain.BlockRef{Number: 10, Hash: domain.Hash{10}, Timestamp: 1700000000}, []CanonicalReceipt{receipt})
		updates = followAll(t, d, time.Now())
		if len(updates) != 1 || !updates[0].FeesBooked || updates[0].First {
			t.Fatal("canonical fees not booked", updates)
		}
		paid := updates[0].Settlement
		if paid.FeeStatus != "" || paid.Net.Cmp(new(big.Int).Sub(wantProfit, big.NewInt(300))) != 0 || paid.Block.Hash != (domain.Hash{10}) || paid.Position == nil {
			t.Fatalf("paid settlement %+v", paid)
		}
		if d.store.state.Charged.Int64() != 300 || len(d.store.state.Included) != 0 || network.receiptReads() != 0 {
			t.Fatal("charge not moved to the paid fee, watch not finished, or receipt read", d.store.state.Charged, network.receiptReads())
		}
	}
}

// A canonical block assembled from the flashblock stream has no fees. It
// still counts as read: the transaction is booked canonical, the missing-block
// check does not fire, and one receipt read supplies the fees.
func TestStreamCanonicalBlockWithoutFeesBooksCanonically(t *testing.T) {
	d, i, network := openSettlementFixture(t)
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	settleFromFeed(t, d, hash, true, observationLogs(i))
	network.addReceipt(paidReceipt(hash, 10, true, observationLogs(i), 400))
	d.ObserveCanonical(domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, []CanonicalReceipt{{Hash: hash, Success: true, Logs: observationLogs(i)}})
	sent := network.sends()
	updates := followAll(t, d, time.Now())
	if len(updates) != 1 || !updates[0].FeesBooked || updates[0].Settlement.Fees.Total.Int64() != 400 {
		t.Fatal("receipt fees not booked after a fee-less canonical block", updates)
	}
	if network.receiptReads() != 1 || network.sends() != sent || len(d.store.state.Included) != 0 || d.store.state.HaltedReason != "" {
		t.Fatal("fee-less canonical sighting did not finish the watch with one receipt read", network.receiptReads(), d.store.state.HaltedReason)
	}
}

// Without a canonical block, one receipt read supplies the fees.
func TestReceiptFollowUpBooksFeesAndAdjustsBudget(t *testing.T) {
	d, i, network := openSettlementFixture(t)
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	settleFromFeed(t, d, hash, true, observationLogs(i))
	network.addReceipt(paidReceipt(hash, 10, true, observationLogs(i), 400))
	updates := followAll(t, d, time.Now())
	if len(updates) != 1 || !updates[0].FeesBooked || updates[0].Settlement.Fees.GasUsed != 15000 {
		t.Fatal(updates)
	}
	if d.store.state.Charged.Int64() != 400 || len(d.store.state.Included) != 0 || network.receiptReads() != 1 {
		t.Fatal("receipt fees not booked once", d.store.state.Charged, network.receiptReads())
	}
}

// A receipt that never arrives leaves the reservation charged and the fee
// unknown; it does not halt.
func TestReceiptNeverArrivingKeepsReservation(t *testing.T) {
	d, i, network := openSettlementFixture(t)
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	settleFromFeed(t, d, hash, true, observationLogs(i))
	updates := followAll(t, d, time.Now())
	if len(updates) != 1 || updates[0].Settlement.FeeStatus != FeesUnknown || updates[0].FeesBooked {
		t.Fatal(updates)
	}
	if d.store.state.Charged.Cmp(i.Budget) != 0 || d.store.state.HaltedReason != "" || network.receiptReads() != len(d.timing.feeAttempts) {
		t.Fatal("unknown fee changed the charge or halted", d.store.state.Charged, d.store.state.HaltedReason)
	}
	if followAll(t, d, time.Now()); network.receiptReads() != len(d.timing.feeAttempts) {
		t.Fatal("fees read again after the bound")
	}
}

// A pending transaction seen nowhere is checked once with a nonce read.
func TestNeverSeenTransactionFallsBackToTheNonce(t *testing.T) {
	t.Run("consumed by an unknown transaction halts", func(t *testing.T) {
		d, i, network := openSettlementFixture(t)
		if _, err := d.Dispatch(t.Context(), i, func() bool { return true }); err != nil {
			t.Fatal(err)
		}
		if followAll(t, d, time.Now().Add(29*time.Second)); network.receiptReads() != 0 {
			t.Fatal("checked before the bound")
		}
		network.set(func(n *dispatchNetworkFixture) { n.latest = 1 })
		followAll(t, d, time.Now().Add(31*time.Second))
		if d.store.state.HaltedReason != "pending nonce consumed by an unrecorded transaction" || network.receiptReads() != 2 {
			t.Fatal(d.store.state.HaltedReason, network.receiptReads())
		}
	})
	t.Run("own receipt on the second look settles", func(t *testing.T) {
		d, i, network := openSettlementFixture(t)
		hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		network.addReceipt(paidReceipt(hash, 10, true, observationLogs(i), 50))
		network.set(func(n *dispatchNetworkFixture) { n.latest, n.hidden = 1, 1 })
		updates := followAll(t, d, time.Now().Add(31*time.Second))
		if len(updates) != 1 || !updates[0].First || !updates[0].FeesBooked || d.Busy() || d.store.state.HaltedReason != "" {
			t.Fatal("own receipt behind the nonce halted dispatch", updates, d.store.state.HaltedReason)
		}
	})
	t.Run("not consumed is rebroadcast once, then halts", func(t *testing.T) {
		d, i, network := openSettlementFixture(t)
		if _, err := d.Dispatch(t.Context(), i, func() bool { return true }); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		followAll(t, d, now.Add(31*time.Second))
		waitSends(t, network, 2)
		if d.store.state.HaltedReason != "" || d.store.state.Pending.Rebroadcasts != 1 {
			t.Fatal("unused nonce not rebroadcast", d.store.state.HaltedReason)
		}
		if followAll(t, d, now.Add(45*time.Second)); network.sends() != 2 {
			t.Fatal("checked again inside the rebroadcast wait")
		}
		followAll(t, d, now.Add(62*time.Second))
		if d.store.state.HaltedReason != "transaction not included after rebroadcast" || network.sends() != 2 {
			t.Fatal(d.store.state.HaltedReason)
		}
	})
	t.Run("an expired conditional transaction frees its nonce", func(t *testing.T) {
		d, i, network := openSettlementFixture(t)
		now := time.Now()
		i.Conditions = &TransactionConditions{TimestampMax: i.Route.Deadline}
		i, err := SealReactiveIntent(t.Context(), i)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Dispatch(t.Context(), i, func() bool { return true }); err != nil {
			t.Fatal(err)
		}
		followAll(t, d, now.Add(31*time.Second))
		state := d.store.state
		if d.Busy() || state.HaltedReason != "" || state.NextNonce != 0 || state.Charged.Sign() != 0 || network.sends() != 1 {
			t.Fatal("expired conditional transaction still holds the lane", state.HaltedReason, state.Pending != nil, state.NextNonce, state.Charged, network.sends())
		}
	})
}

// waitSends waits for an asynchronous rebroadcast to reach the network.
func waitSends(t *testing.T, network *dispatchNetworkFixture, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for network.sends() < want {
		if time.Now().After(deadline) {
			t.Fatalf("sends %d, want %d", network.sends(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A journal written before this change holds a released entry with no booked
// outcome and a pending one. Both resolve after a restart from receipts.
func TestRestartResolvesReleasedAndPendingEntries(t *testing.T) {
	i, l, s := dispatchFixture(t)
	l.PerDay = new(big.Int).Mul(l.PerTransaction, big.NewInt(10))
	network := &dispatchNetworkFixture{}
	path := filepath.Join(t.TempDir(), "state.json")
	d, err := OpenDispatcher(t.Context(), path, l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	d.ObserveIncluded([]domain.Hash{first})
	second := another(t, i)
	secondHash, err := d.Dispatch(t.Context(), second, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	d, err = OpenDispatcher(t.Context(), path, l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	quickFollowUps(d)
	network.set(func(n *dispatchNetworkFixture) { n.latest = 2 })
	network.addReceipt(paidReceipt(first, 10, true, observationLogs(i), 70))
	network.addReceipt(paidReceipt(secondHash, 11, false, nil, 80))
	updates := followAll(t, d, time.Now().Add(31*time.Second))
	if len(updates) != 2 || !updates[0].FeesBooked || !updates[1].FeesBooked {
		t.Fatal(updates)
	}
	if d.Busy() || len(d.store.state.Included) != 0 || d.store.state.NextNonce != 2 || d.store.state.Charged.Int64() != 150 {
		t.Fatal("journal entries not resolved after restart", d.store.state.Charged, d.store.state.HaltedReason)
	}
}

// A canonical block overrules the feed: a different variant voids the one the
// feed booked.
func TestCanonicalVariantReplacesFeedBooking(t *testing.T) {
	d, i, _ := openSettlementFixture(t)
	original, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := d.Replace(t.Context(), improvedIntent(t, i), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	settleFromFeed(t, d, replacement, false, nil)
	d.ObserveCanonical(domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, []CanonicalReceipt{{Hash: original, Success: true, Logs: observationLogs(i), Fees: &domain.ReceiptFees{Total: big.NewInt(9)}}})
	updates := followAll(t, d, time.Now())
	if len(updates) != 1 || updates[0].Voided != replacement || updates[0].Settlement.Hash != original || !updates[0].Settlement.Success {
		t.Fatal("canonical variant did not replace the feed booking", updates)
	}
	if len(d.store.state.Included) != 0 || d.store.state.Charged.Int64() != 9 {
		t.Fatal(d.store.state.Charged)
	}
}

// blockingReceipts holds every receipt read until released.
type blockingReceipts struct {
	*dispatchNetworkFixture
	started, release chan struct{}
}

func (n *blockingReceipts) Receipt(ctx context.Context, hash domain.Hash) (*rpc.TransactionReceipt, error) {
	n.started <- struct{}{}
	<-n.release
	return n.dispatchNetworkFixture.Receipt(ctx, hash)
}

func TestSendIsNotBlockedByAReceiptRead(t *testing.T) {
	i, l, s := dispatchFixture(t)
	l.PerDay = new(big.Int).Mul(l.PerTransaction, big.NewInt(10))
	network := &blockingReceipts{dispatchNetworkFixture: &dispatchNetworkFixture{}, started: make(chan struct{}, 1), release: make(chan struct{})}
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "state.json"), l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	quickFollowUps(d)
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	settleFromFeed(t, d, hash, true, observationLogs(i))
	if _, err := d.Watch(time.Now()); err != nil {
		t.Fatal(err)
	}
	<-network.started
	next := another(t, i)
	sent := make(chan error, 1)
	go func() {
		_, err := d.Dispatch(t.Context(), next, func() bool { return true })
		sent <- err
	}()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send waited for a receipt read")
	}
	close(network.release)
}
