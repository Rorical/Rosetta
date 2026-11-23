package exec

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// settleTiming bounds how long settlement waits before it spends RPC.
type settleTiming struct {
	neverSeen       time.Duration   // pending, in neither the feed nor a canonical block
	unconfirmed     time.Duration   // booked, but no canonical sighting
	rebroadcastWait time.Duration   // from a rebroadcast to the next check
	retry           time.Duration   // after a failed check
	recheck         time.Duration   // between receipt reads when the nonce moved without one
	feeAttempts     []time.Duration // receipt reads after booking; the last bounds the wait
	rpc             time.Duration   // per call
}

var defaultSettleTiming = settleTiming{neverSeen: 30 * time.Second, unconfirmed: time.Minute, rebroadcastWait: 30 * time.Second,
	retry: 10 * time.Second, recheck: 2 * time.Second, rpc: 10 * time.Second,
	// A canonical block normally delivers the fees about two seconds after the
	// feed, before the first read.
	feeAttempts: []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 2 * time.Minute}}

type followKind uint8

const (
	followFees followKind = iota
	followVerify
	followDiagnose
)

type taskKey struct {
	nonce uint64
	kind  followKind
}

// Followup is a finished follow-up task. Its owner passes it to ApplyFollowup
// on the goroutine that sends.
type Followup struct {
	kind      followKind
	nonce     uint64
	hash      domain.Hash
	receipt   *rpc.TransactionReceipt
	consumed  bool // the latest nonce passed this one
	unknown   bool // no receipt within the fee bound
	diagnosis string
	err       error
}

// Followups delivers finished follow-up tasks.
func (d *Dispatcher) Followups() <-chan Followup { return d.results }

// Watch books canonical sightings and starts the follow-ups that are due. It
// makes no RPC; call it periodically from the goroutine that sends.
func (d *Dispatcher) Watch(now time.Time) ([]SettlementUpdate, error) {
	sightings, from, to := d.canonical.take()
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.store.state
	changed := false
	var updates []SettlementUpdate
	for _, seen := range sightings {
		receipt := &rpc.TransactionReceipt{Hash: seen.receipt.Hash, Block: seen.block, Success: seen.receipt.Success, Logs: seen.receipt.Logs, Fees: seen.receipt.Fees}
		next, booked, ok := d.bookCanonical(state, receipt, now)
		if ok {
			state, changed = next, true
			updates = append(updates, booked...)
		}
	}
	if state.HaltedReason == "" {
		var rebroadcast bool
		state, rebroadcast = d.checkMissing(state, from, to, now)
		changed = changed || rebroadcast
	}
	d.followFees(state)
	if !changed {
		return updates, nil
	}
	return updates, d.commit(state)
}

// bookCanonical books a receipt from a canonical block or the node. An
// executor event that does not verify halts: the outcome cannot be booked.
func (d *Dispatcher) bookCanonical(state DispatchState, receipt *rpc.TransactionReceipt, now time.Time) (DispatchState, []SettlementUpdate, bool) {
	state, index, variant, ok := claim(state, receipt.Hash, now)
	if !ok {
		return state, nil, false
	}
	settled, err := settleExecutor(variant, receipt)
	if err != nil {
		if state.HaltedReason == "" {
			state.HaltedReason = fmt.Sprintf("canonical receipt %s: %v", receipt.Hash.Hex(), err)
		}
		return state, nil, true
	}
	return state, d.book(&state, index, variant, settled, true, now), true
}

// checkMissing looks for nonces the chain may have lost. A booked transaction
// absent from the canonical block it was seen in is rebroadcast at once; any
// other doubt costs a nonce read after a bound. It reports state changes.
func (d *Dispatcher) checkMissing(state DispatchState, from, to uint64, now time.Time) (DispatchState, bool) {
	nowMS := now.UnixMilli()
	if p := state.Pending; p != nil && !d.Included() && nowMS >= max(p.SubmittedMS+d.timing.neverSeen.Milliseconds(), p.CheckMS) {
		d.verify(*p)
	}
	changed := false
	state.Included = slices.Clone(state.Included)
	for i := range state.Included {
		entry := &state.Included[i]
		if entry.Canonical || d.running[taskKey{entry.Nonce, followVerify}] {
			continue
		}
		if entry.Settlement != nil && from != 0 && from <= entry.ObservedBlock && entry.ObservedBlock <= to {
			// The follower read the block it was seen in, and it was not there.
			if entry.Rebroadcasts == 0 {
				changed = d.rebroadcast(entry, now) || changed
			} else if nowMS >= entry.CheckMS {
				d.verify(*entry)
			}
			continue
		}
		base, wait := entry.ReleasedMS, d.timing.neverSeen
		if entry.Settlement != nil {
			base, wait = entry.SettledMS, d.timing.unconfirmed
		}
		if nowMS >= max(base+wait.Milliseconds(), entry.CheckMS) {
			d.verify(*entry)
		}
	}
	return state, changed
}

// rebroadcast resends the booked variant, or the newest one, with its exact
// signed bytes, so later nonces are not stranded behind a gap. A late arrival
// reverts at the executor's deadline or profit check and only costs gas. The
// stop file blocks it like any other send; the caller checks for a halt.
func (d *Dispatcher) rebroadcast(entry *PendingTransaction, now time.Time) bool {
	if stopped, err := d.stopRequested(); err != nil || stopped {
		return false
	}
	raw := entry.Raw
	if entry.Settlement != nil {
		if variant, ok := variantOf(*entry, entry.Settlement.Hash); ok {
			raw = variant.Raw
		}
	}
	entry.Rebroadcasts++
	entry.CheckMS = now.Add(d.timing.rebroadcastWait).UnixMilli()
	d.tasks.Go(func() {
		ctx, cancel := context.WithTimeout(d.ctx, d.timing.rpc)
		defer cancel()
		// "nonce too low" or "already known" means it is on chain after all;
		// the next check finds it. A conditional transaction keeps its
		// conditions: the hot path relies on them for the pause.
		d.network.Send(ctx, raw, entry.Intent.Conditions)
	})
	return true
}

func (d *Dispatcher) followFees(state DispatchState) {
	for _, entry := range state.Included {
		settled := entry.Settlement
		if settled == nil || settled.FeeStatus != FeesPending {
			continue
		}
		nonce, hash, settledMS := entry.Nonce, settled.Hash, entry.SettledMS
		d.launch(taskKey{nonce, followFees}, func(ctx context.Context) Followup { return d.fetchFees(ctx, nonce, hash, settledMS) })
	}
}

func (d *Dispatcher) verify(entry PendingTransaction) {
	d.launch(taskKey{entry.Nonce, followVerify}, func(ctx context.Context) Followup { return d.verifyNonce(ctx, entry) })
}

func (d *Dispatcher) diagnose(variant PendingTransaction, settled Settlement) {
	d.launch(taskKey{variant.Nonce, followDiagnose}, func(ctx context.Context) Followup {
		ctx, cancel := context.WithTimeout(ctx, d.timing.rpc)
		defer cancel()
		return Followup{hash: settled.Hash, diagnosis: d.network.Diagnose(ctx, variant.Intent, &settled)}
	})
}

// launch runs one task per key off the send path. It owns d.mu for running;
// the task itself never takes the lock.
func (d *Dispatcher) launch(key taskKey, run func(context.Context) Followup) {
	if d.running[key] {
		return
	}
	d.running[key] = true
	d.tasks.Go(func() {
		result := run(d.ctx)
		result.kind, result.nonce = key.kind, key.nonce
		select {
		case d.results <- result:
		case <-d.ctx.Done():
		}
	})
}

// fetchFees reads the booked variant's receipt once it should exist, unless a
// canonical block delivered the fees first. After the last attempt the fees
// are reported unknown.
func (d *Dispatcher) fetchFees(ctx context.Context, nonce uint64, hash domain.Hash, settledMS int64) Followup {
	result := Followup{hash: hash}
	for _, offset := range d.timing.feeAttempts {
		if !sleep(ctx, time.Until(time.UnixMilli(settledMS).Add(offset))) || !d.feesPending(nonce) {
			return result
		}
		// A receipt without parseable fees cannot end the wait.
		if receipt, err := d.receipt(ctx, hash); err == nil && receipt != nil && receipt.Fees != nil {
			result.receipt = receipt
			return result
		}
	}
	result.unknown = true
	return result
}

// verifyNonce asks the chain what became of a nonce settlement lost track of:
// one nonce read, then receipt reads only if the nonce moved.
func (d *Dispatcher) verifyNonce(ctx context.Context, entry PendingTransaction) Followup {
	var result Followup
	call, cancel := context.WithTimeout(ctx, d.timing.rpc)
	latest, err := d.network.ConfirmedNonce(call, d.limits.Sender)
	cancel()
	if err != nil || latest <= entry.Nonce {
		result.err = err
		return result
	}
	result.consumed = true
	// A load-balanced node can answer the nonce before it serves the receipt.
	for attempt := range 2 {
		if attempt > 0 && !sleep(ctx, d.timing.recheck) {
			result.err = ctx.Err()
			return result
		}
		for _, variant := range variants(entry) {
			receipt, err := d.receipt(ctx, variant.Hash)
			if err != nil {
				result.err = err
				return result
			}
			if receipt != nil {
				result.hash, result.receipt = variant.Hash, receipt
				return result
			}
		}
	}
	return result
}

func (d *Dispatcher) receipt(ctx context.Context, hash domain.Hash) (*rpc.TransactionReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, d.timing.rpc)
	defer cancel()
	receipt, err := d.network.Receipt(ctx, hash)
	if err == nil && receipt != nil && receipt.Hash != hash {
		return nil, fmt.Errorf("receipt for %s answered %s", hash.Hex(), receipt.Hash.Hex())
	}
	return receipt, err
}

// ApplyFollowup books a finished follow-up. Call it from the goroutine that
// sends; it makes no RPC.
func (d *Dispatcher) ApplyFollowup(f Followup, now time.Time) ([]SettlementUpdate, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, taskKey{f.nonce, f.kind})
	state := d.store.state
	if f.receipt != nil {
		next, updates, ok := d.bookCanonical(state, f.receipt, now)
		if !ok {
			return nil, nil
		}
		return updates, d.commit(next)
	}
	switch f.kind {
	case followDiagnose:
		if f.diagnosis == "" {
			return nil, nil
		}
		return []SettlementUpdate{{Diagnosis: &RevertDiagnosis{Hash: f.hash, Diagnosis: f.diagnosis}}}, nil
	case followFees:
		return d.applyUnknownFees(state, f, now)
	}
	return nil, d.applyVerify(state, f, now)
}

// applyUnknownFees keeps the reservation charged for a receipt that never came.
func (d *Dispatcher) applyUnknownFees(state DispatchState, f Followup, now time.Time) ([]SettlementUpdate, error) {
	index := slices.IndexFunc(state.Included, func(entry PendingTransaction) bool { return entry.Nonce == f.nonce })
	if !f.unknown || index < 0 {
		return nil, nil
	}
	entry := state.Included[index]
	if entry.Settlement == nil || entry.Settlement.Hash != f.hash || entry.Settlement.FeeStatus != FeesPending {
		return nil, nil
	}
	variant, _ := variantOf(entry, f.hash)
	unknown := *entry.Settlement
	unknown.FeeStatus = FeesUnknown
	state.Included = slices.Clone(state.Included)
	updates := d.book(&state, index, variant, &unknown, false, now)
	return updates, d.commit(state)
}

// applyVerify acts on a nonce check that found no receipt of ours.
func (d *Dispatcher) applyVerify(state DispatchState, f Followup, now time.Time) error {
	entry, pending := locate(state, f.nonce)
	if entry == nil || entry.Canonical || state.HaltedReason != "" {
		return nil
	}
	switch {
	case f.err != nil:
		entry.CheckMS = now.Add(d.timing.retry).UnixMilli()
	case f.consumed:
		// A variant broadcast while its journal write was in flight can
		// consume the nonce without being recorded; an operator reconciles it.
		state.HaltedReason = "pending nonce consumed by an unrecorded transaction"
	case pending && conditionsExpired(*entry, now):
		// The sequencer can no longer include any variant, so the nonce and
		// the reservation are free again, as after a rejected send.
		state.Pending = nil
		state.Charged = new(big.Int).Sub(state.Charged, pendingBudget(*entry))
		if state.Charged.Sign() < 0 {
			state.Charged = new(big.Int)
		}
		slog.Warn("expired conditional transaction released its nonce", "nonce", entry.Nonce, "hash", entry.Hash.Hex())
		return d.commit(state)
	case entry.Rebroadcasts == 0:
		if !d.rebroadcast(entry, now) {
			entry.CheckMS = now.Add(d.timing.rebroadcastWait).UnixMilli()
		}
	case pending:
		state.HaltedReason = "transaction not included after rebroadcast"
	default:
		state.HaltedReason = "released transaction missing from the chain after rebroadcast"
	}
	return d.commit(replace(state, *entry, pending))
}

// conditionExpiryMargin is how far the wall clock must be past a condition's
// L2 timestamp bound before the transaction counts as dead: the sequencer
// stamps blocks from its own clock.
const conditionExpiryMargin = 15 * time.Second

// conditionsExpired reports that every signed variant of entry was sent with
// conditions whose timestamp bound has passed. The sequencer checks the bound
// when it sequences, so none of them can still consume the nonce.
func conditionsExpired(entry PendingTransaction, now time.Time) bool {
	for _, variant := range variants(entry) {
		c := variant.Intent.Conditions
		if c == nil || c.TimestampMax == 0 || !now.After(time.Unix(int64(c.TimestampMax), 0).Add(conditionExpiryMargin)) {
			return false
		}
	}
	return true
}

// locate returns a copy of the unfinished transaction holding nonce.
func locate(state DispatchState, nonce uint64) (*PendingTransaction, bool) {
	if p := state.Pending; p != nil && p.Nonce == nonce {
		entry := *p
		return &entry, true
	}
	for _, entry := range state.Included {
		if entry.Nonce == nonce {
			return &entry, false
		}
	}
	return nil, false
}

func replace(state DispatchState, entry PendingTransaction, pending bool) DispatchState {
	if pending {
		state.Pending = &entry
		return state
	}
	state.Included = slices.Clone(state.Included)
	for i := range state.Included {
		if state.Included[i].Nonce == entry.Nonce {
			state.Included[i] = entry
		}
	}
	return state
}

func sleep(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
