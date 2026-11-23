package exec

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// another returns a second sealed intent for the same route.
func another(t *testing.T, i ReactiveIntent) ReactiveIntent {
	t.Helper()
	i.CreatedMS-- // a distinct identity with the same route and economics
	sealed, err := SealReactiveIntent(t.Context(), i)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestIncludedNonceReleasesTheLaneAndSettlesLater(t *testing.T) {
	i, l, s := dispatchFixture(t)
	l.PerDay = new(big.Int).Mul(l.PerTransaction, big.NewInt(10)) // several sends in flight
	network := &dispatchNetworkFixture{}
	path := filepath.Join(t.TempDir(), "state.json")
	d, err := OpenDispatcher(t.Context(), path, l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	always := func() bool { return true }
	first, err := d.Dispatch(t.Context(), i, always)
	if err != nil {
		t.Fatal(err)
	}
	nonce := d.Pending().Nonce
	if _, err := d.Dispatch(t.Context(), i, always); !errors.Is(err, ErrDispatchBusy) {
		t.Fatal("lane opened before the first transaction was seen included", err)
	}
	d.ObserveIncluded([]domain.Hash{first})
	if _, err := d.Dispatch(t.Context(), i, always); !errors.Is(err, ErrDispatchRejected) {
		t.Fatal("an intent already in flight was signed again", err)
	}
	if _, err := d.Dispatch(t.Context(), another(t, i), always); err != nil {
		t.Fatal("included nonce did not release the lane", err)
	}
	if p := d.Pending(); p == nil || p.Nonce != nonce+1 || len(d.store.state.Included) != 1 || d.store.state.Included[0].Hash != first {
		t.Fatal("next send did not take the next nonce or lost the released one")
	}

	// A restart while the released nonce is not yet in the latest block resumes.
	d.Close()
	network.latest = nonce
	d, err = OpenDispatcher(t.Context(), path, l, s, network)
	if err != nil {
		t.Fatal("restart refused a released nonce still absent from the latest block", err)
	}
	defer d.Close()

	// A canonical block settles the released transaction without touching the lane.
	d.ObserveCanonical(domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, []CanonicalReceipt{{Hash: first, Success: true, Logs: observationLogs(i), Fees: &domain.ReceiptFees{Total: big.NewInt(1)}}})
	updates := followAll(t, d, time.Now())
	if len(updates) != 1 || updates[0].Settlement.Hash != first || !updates[0].Settlement.Success {
		t.Fatal("released transaction did not settle", updates)
	}
	if len(d.store.state.Included) != 0 || d.Pending() == nil || d.Pending().Nonce != nonce+1 || d.store.state.NextNonce != nonce+1 {
		t.Fatal("settling the released nonce disturbed the pending one")
	}
}

// A booked transaction absent from the canonical block it was seen in was
// reorganized away. It is rebroadcast once; a later canonical sighting ends
// the watch, and a nonce still unused after the bound halts.
func TestReorgedSettlementIsRebroadcastThenConfirmedOrHalted(t *testing.T) {
	for _, recovered := range []bool{true, false} {
		i, l, s := dispatchFixture(t)
		l.PerDay = new(big.Int).Mul(l.PerTransaction, big.NewInt(10))
		network := &dispatchNetworkFixture{}
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
		if _, err := d.SettleObserved(d.ObserveExecution(hash, true, observationLogs(i), ExecutionPosition{BlockNumber: 10}), time.Now()); err != nil {
			t.Fatal(err)
		}
		sent := network.sends()
		now := time.Now()
		d.ObserveCanonical(domain.BlockRef{Number: 9}, nil)
		if followAll(t, d, now); network.sends() != sent {
			t.Fatal("rebroadcast before the canonical chain reached its block")
		}
		// The kill switch stops rebroadcasts like any other send.
		if err := os.WriteFile(l.StopFile, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		d.ObserveCanonical(domain.BlockRef{Number: 10}, nil)
		if followAll(t, d, now); network.sends() != sent {
			t.Fatal("rebroadcast despite the stop file")
		}
		os.Remove(l.StopFile)
		if followAll(t, d, now); network.sends() != sent+1 || d.store.state.Included[0].Rebroadcasts != 1 {
			t.Fatal("reorganized transaction was not rebroadcast", network.sends()-sent)
		}
		if followAll(t, d, now); network.sends() != sent+1 {
			t.Fatal("rebroadcast twice")
		}
		if recovered {
			fees := &domain.ReceiptFees{Total: big.NewInt(5)}
			d.ObserveCanonical(domain.BlockRef{Number: 11, Hash: domain.Hash{11}}, []CanonicalReceipt{{Hash: hash, Success: true, Logs: observationLogs(i), Fees: fees}})
			followAll(t, d, now.Add(time.Minute))
			if len(d.store.state.Included) != 0 || d.store.state.HaltedReason != "" {
				t.Fatal("canonical sighting after the rebroadcast did not finish the watch", d.store.state.HaltedReason)
			}
			continue
		}
		followAll(t, d, now.Add(time.Minute))
		if d.store.state.HaltedReason != "released transaction missing from the chain after rebroadcast" || network.sends() != sent+1 {
			t.Fatal("missing transaction did not halt", d.store.state.HaltedReason)
		}
	}
}

func TestNewDayKeepsInFlightReservations(t *testing.T) {
	i, l, s := dispatchFixture(t)
	l.PerDay = new(big.Int).Mul(l.PerTransaction, big.NewInt(10))
	network := &dispatchNetworkFixture{}
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "state.json"), l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	always := func() bool { return true }
	first, err := d.Dispatch(t.Context(), i, always)
	if err != nil {
		t.Fatal(err)
	}
	d.ObserveIncluded([]domain.Hash{first})
	d.store.state.Day-- // the released transaction was reserved yesterday
	second := another(t, i)
	if _, err := d.Dispatch(t.Context(), second, always); err != nil {
		t.Fatal(err)
	}
	want := new(big.Int).Add(i.Budget, second.Budget)
	if d.store.state.Charged.Cmp(want) != 0 {
		t.Fatalf("new day charged %s, want the in-flight %s", d.store.state.Charged, want)
	}
}
