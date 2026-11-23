package exec

import (
	"math/big"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A transaction settles when one of its variants first appears in the
// flashblocks feed or in a canonical block. The feed gives the outcome
// and executor profit; paid fees follow from the canonical block's receipts or
// one receipt read. A canonical block or receipt overrules the feed.

// SettlementUpdate is one change to a nonce's booked outcome, for the output.
type SettlementUpdate struct {
	Settlement *Settlement // nil for a diagnosis
	// Voided is an earlier booked variant that canonical evidence replaced.
	Voided domain.Hash
	// First is the nonce's first booked outcome. FeesBooked means paid fees
	// became known with this update.
	First, FeesBooked bool
	Diagnosis         *RevertDiagnosis
}

// RevertDiagnosis is the trace explanation of a canonical revert.
type RevertDiagnosis struct {
	Hash      domain.Hash `json:"transaction"`
	Diagnosis string      `json:"diagnosis"`
}

// SettleObserved books a feed sighting of one of our transactions. The nonce
// is consumed, so the lane stays open and the outcome stands unless a
// canonical block disagrees. It makes no RPC.
func (d *Dispatcher) SettleObserved(o *ExecutionObservation, now time.Time) ([]SettlementUpdate, error) {
	if o == nil || o.Error != "" {
		// An executor event that did not verify waits for the canonical receipt.
		return nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	state, index, variant, ok := claim(d.store.state, o.Hash, now)
	if !ok {
		return nil, nil
	}
	position := o.Position
	observed := &Settlement{Hash: o.Hash, IntentID: variant.Intent.ID, Block: domain.BlockRef{Number: position.BlockNumber}, Position: &position,
		Success: o.Success, Cancelled: o.Cancelled, Maintenance: o.Maintenance, Asset: variant.Intent.Route.Asset, Profit: zeroIfNil(o.Profit), LoanFee: zeroIfNil(o.LoanFee),
		QuoteError: o.QuoteError, FeeStatus: FeesPending, Basis: "flashblocks_feed; admission_conversion"}
	updates := d.book(&state, index, variant, observed, false, now)
	return updates, d.commit(state)
}

// claim finds the unfinished nonce that hash belongs to and returns a state
// whose Included list the caller may modify. A pending nonce is released.
func claim(state DispatchState, hash domain.Hash, now time.Time) (DispatchState, int, PendingTransaction, bool) {
	if p := state.Pending; p != nil {
		if variant, ok := variantOf(*p, hash); ok {
			state = releaseIncluded(state, now)
			return state, len(state.Included) - 1, variant, true
		}
	}
	for index, entry := range state.Included {
		if variant, ok := variantOf(entry, hash); ok {
			state.Included = slices.Clone(state.Included)
			return state, index, variant, true
		}
	}
	return state, 0, PendingTransaction{}, false
}

// book merges new evidence about the released nonce at index into its booked
// outcome and the day's charge. Canonical evidence comes from a canonical
// block or a receipt. Paid fees replace the reservation in the charge, and
// fees above the reservation halt.
func (d *Dispatcher) book(state *DispatchState, index int, variant PendingTransaction, next *Settlement, canonical bool, now time.Time) []SettlementUpdate {
	entry := &state.Included[index]
	prev := entry.Settlement
	if prev != nil && entry.Canonical && !canonical && !sameOutcome(prev, next) {
		return nil
	}
	merged := *next
	merged.Intent = nil
	if prev != nil && prev.Hash == merged.Hash {
		if prev.Fees != nil {
			// The same transaction's fees cannot change; price succeeded on
			// them with this variant's conversion before.
			merged.Fees = prev.Fees
			_ = merged.price(variant.Intent.Conversion)
		} else if merged.Fees == nil && prev.FeeStatus == FeesUnknown {
			merged.FeeStatus = FeesUnknown
		}
		if merged.Position == nil {
			merged.Position = prev.Position
		}
		if merged.Block.Hash == (domain.Hash{}) && prev.Block.Hash != (domain.Hash{}) {
			merged.Block = prev.Block
		}
	}
	first := prev == nil
	if first {
		// Roll the day while this nonce still counts as in flight, so its
		// reservation is charged to the day it settles.
		*state = rollDay(*state, now)
		entry = &state.Included[index]
		entry.SettledMS = now.UnixMilli()
		for _, v := range variants(*entry) {
			state.Completed = appendBounded(state.Completed, v.Intent.ID)
		}
	}
	reserved := pendingBudget(*entry)
	entry.Settlement = &merged
	if next.Block.Number != 0 {
		entry.ObservedBlock = next.Block.Number
	}
	becameCanonical := canonical && !entry.Canonical
	entry.Canonical = entry.Canonical || canonical
	charge(state, entry.SettledMS, reserved, paid(prev, reserved), paid(&merged, reserved), now)

	var updates []SettlementUpdate
	outcomeChanged := first || !sameOutcome(prev, &merged)
	if outcomeChanged || prev.FeeStatus != merged.FeeStatus {
		published := merged
		published.Intent = &variant.Intent
		update := SettlementUpdate{Settlement: &published, First: first, FeesBooked: merged.Fees != nil && (first || prev.Fees == nil || prev.Hash != merged.Hash)}
		if !first && prev.Hash != merged.Hash {
			update.Voided = prev.Hash
		}
		state.LastReceipt = &published
		updates = append(updates, update)
	}
	if canonical && (becameCanonical || outcomeChanged) && !merged.Success && !merged.Cancelled && merged.Maintenance == "" {
		d.diagnose(variant, merged)
	}
	return updates
}

func sameOutcome(a, b *Settlement) bool {
	return a.Hash == b.Hash && a.Success == b.Success && a.Cancelled == b.Cancelled &&
		zeroIfNil(a.Profit).Cmp(zeroIfNil(b.Profit)) == 0 && zeroIfNil(a.LoanFee).Cmp(zeroIfNil(b.LoanFee)) == 0
}

// paid is what a nonce costs the day: the reservation until paid fees are known.
func paid(s *Settlement, reserved *big.Int) *big.Int {
	if s == nil || s.Fees == nil {
		return reserved
	}
	return s.Fees.Total
}

// charge moves one nonce's part of the day's charge from before to after. A
// refund only applies on the day the nonce settled; an excess always counts.
func charge(state *DispatchState, settledMS int64, reserved, before, after *big.Int, now time.Time) {
	if after.Cmp(reserved) > 0 && state.HaltedReason == "" {
		state.HaltedReason = "paid fees exceeded reserved budget"
	}
	delta := new(big.Int).Sub(after, before)
	if delta.Sign() == 0 {
		return
	}
	*state = rollDay(*state, now)
	if delta.Sign() < 0 && settledMS/86400000 != state.Day {
		return
	}
	state.Charged = new(big.Int).Add(state.Charged, delta)
	if state.Charged.Sign() < 0 {
		state.Charged.SetInt64(0)
	}
}

// rollDay starts a new day's charge from the reservations still in flight.
func rollDay(state DispatchState, now time.Time) DispatchState {
	if day := now.Unix() / 86400; day > state.Day {
		state.Day, state.Charged = day, inFlight(state)
	}
	return state
}

// finished nonces have a canonical sighting and final fees, and leave the journal.
func finished(entry PendingTransaction) bool {
	return entry.Canonical && entry.Settlement != nil && entry.Settlement.FeeStatus != FeesPending
}

// commit drops finished nonces, saves, and publishes the lock-free views.
func (d *Dispatcher) commit(state DispatchState) error {
	state.Included = slices.DeleteFunc(slices.Clone(state.Included), finished)
	if err := d.store.save(state); err != nil {
		return err
	}
	d.publish(state)
	return nil
}

// watchSet holds every signed variant of the unfinished nonces, so the feed
// and the canonical follower can match our transactions without the lock.
type watchSet struct {
	variants map[domain.Hash]PendingTransaction
	fees     map[uint64]bool // nonces whose paid fees are still pending
}

func (d *Dispatcher) publish(state DispatchState) {
	d.pending.Store(state.Pending)
	w := &watchSet{variants: map[domain.Hash]PendingTransaction{}, fees: map[uint64]bool{}}
	add := func(entry PendingTransaction) {
		for _, variant := range variants(entry) {
			w.variants[variant.Hash] = variant
		}
		if entry.Settlement != nil && entry.Settlement.FeeStatus == FeesPending {
			w.fees[entry.Nonce] = true
		}
	}
	for _, entry := range state.Included {
		add(entry)
	}
	if state.Pending != nil {
		add(*state.Pending)
	}
	d.watch.Store(w)
}

// Watched returns the signed variant with this hash of an unfinished nonce.
func (d *Dispatcher) Watched(hash domain.Hash) (PendingTransaction, bool) {
	w := d.watch.Load()
	if w == nil {
		return PendingTransaction{}, false
	}
	variant, ok := w.variants[hash]
	return variant, ok
}

func (d *Dispatcher) feesPending(nonce uint64) bool {
	w := d.watch.Load()
	return w != nil && w.fees[nonce]
}

func zeroIfNil(value *big.Int) *big.Int {
	if value == nil {
		return new(big.Int)
	}
	return value
}
