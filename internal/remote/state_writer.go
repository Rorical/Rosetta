package remote

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func (r *runtime) reader() *snapshot {
	if r.canonicalReader == nil || r.canonicalReader.block.Ref != r.block.Ref {
		if r.canonicalReader != nil {
			r.canonicalReader.Close()
		}
		r.canonicalReader = &snapshot{client: r.client, block: r.block, chain: domain.ChainIdentity{Family: r.cfg.Network.Family, ChainID: r.cfg.ChainID()}, deployments: r.cfg.Network.DEX}
	}
	return r.canonicalReader.Retain().(*snapshot)
}
func (r *runtime) advance() {
	r.reactiveRevision.Add(1)
	r.epoch++
	r.version = domain.StateVersion{Source: domain.SourceStandard, Anchor: r.block.Ref, Epoch: r.epoch, BlockNumber: r.block.Ref.Number, PrefixHash: r.block.Ref.Hash}
	r.planPending = false
	if r.confirmation != nil {
		r.confirmation.Invalidate()
	}
}

func (r *runtime) acceptCanonical(ctx context.Context, update canonicalUpdate) error {
	if at, ok := r.feedArrivals.arrival(update.block.Ref.Hash); ok {
		r.metrics.Observe(telemetry.Standard, telemetry.FeedToApply, time.Since(at))
		r.feedArrivals.learn(update.block.Ref.Hash, update.block.Ref.Number)
	}
	r.nearTip = update.nearTip
	r.canonicalLag = 0
	if update.tip > update.block.Ref.Number {
		r.canonicalLag = update.tip - update.block.Ref.Number
	}
	r.observeReactiveCanonical(update)
	if update.reset == nil && r.research != nil {
		if update.catchingUp && !update.nearTip {
			r.research.ObserveReceipts(r.store.View(), update.block.Ref, update.transactions)
		} else {
			r.research.ObserveBlock(r.store.View(), update.block.Ref, update.transactions, update.fees...)
		}
	}
	if update.nextPools != nil {
		defer func() { update.nextPools <- r.store.Pools() }()
	}
	if r.ticks != nil {
		r.ticks.observe(update)
	}
	var dirty []domain.PoolKey
	var logCount uint64
	if update.reset != nil {
		if r.reactive != nil && r.reactive.quiet != nil {
			r.reactive.quiet.reset()
		}
		r.store = update.reset
		r.rescreen = true
		r.waitingFees = make(map[domain.PoolKey]bool)
		for _, p := range r.store.Pools() {
			if pipeline.DynamicFee(p.Key.Kind) && p.Slipstream == nil {
				r.waitingFees[p.Key] = true
			}
		}
	} else {
		branch, err := ingest.NewTransactionBranch(r.store)
		if err != nil {
			return err
		}
		if err := branch.Begin(update.block.Ref); err != nil {
			return err
		}
		// Without a preconfirmation stream, canonical blocks are the only
		// view of rivals; blocks replayed while catching up are not current.
		rivals := r.reactive != nil && r.reactive.competition != nil && r.cfg.FastURL == "" && r.cfg.EventSource != "flashblocks" && update.nearTip && !update.catchingUp
		received := time.Now()
		for _, transaction := range update.transactions {
			logCount += uint64(len(transaction.Logs))
			changed, err := branch.Apply(ctx, transaction)
			if err != nil {
				return err
			}
			dirty = append(dirty, changed...)
			if rivals && len(changed) >= 2 {
				r.reactive.competition.observeCanonical(update.block.Ref.Number, transaction, changed, received)
			}
		}
		if err := branch.Commit(update.block.Ref, update.block.Transactions); err != nil {
			return err
		}
		r.store = branch.Base()
		dirty = append(dirty, r.store.Dirty()...)
		if r.reactive != nil && r.reactive.quiet != nil {
			r.reactive.quiet.observeBlock(update.block.Ref, update.transactions)
		}
	}
	repaired, err := applyMetricReads(r.store, update.metric, update.block.Ref)
	if err != nil {
		return err
	}
	dirty = append(dirty, repaired...)
	var feeUpdates []pool.State
	for _, observed := range update.fees {
		current, ok := r.store.Pool(observed.Key)
		if !ok {
			continue
		}
		sameReverse := current.FeeOneForZero == nil && observed.FeeOneForZero == nil || current.FeeOneForZero != nil && observed.FeeOneForZero != nil && *current.FeeOneForZero == *observed.FeeOneForZero
		changed := observed.Slipstream != nil || current.Fee != observed.Fee || current.ProtocolFee != observed.ProtocolFee || !sameReverse
		if changed || r.waitingFees[observed.Key] {
			dirty = append(dirty, observed.Key)
		}
		if changed {
			current.Fee, current.ProtocolFee, current.FeeOneForZero = observed.Fee, observed.ProtocolFee, observed.FeeOneForZero
			if observed.Slipstream != nil {
				current.Slipstream = observed.Slipstream
			}
			if err := r.store.Upsert(current); err != nil {
				return err
			}
		}
		feeUpdates = append(feeUpdates, current)
		delete(r.waitingFees, observed.Key)
	}
	if update.reset != nil {
		if err := r.capture.Snapshot(r.store.View()); err != nil {
			return err
		}
	} else if err := r.capture.Block(update.block.Ref, update.transactions); err != nil {
		return err
	}
	if len(feeUpdates) > 0 {
		if err := r.capture.Dependencies(update.block.Ref, feeUpdates, nil); err != nil {
			return err
		}
	}
	slices.SortFunc(dirty, domain.ComparePoolKeys)
	dirty = slices.Compact(dirty)
	metrics := telemetry.FromContext(ctx)
	metrics.Add(telemetry.Standard, telemetry.Messages, 1)
	metrics.Add(telemetry.Standard, telemetry.Receipts, uint64(len(update.transactions)))
	metrics.Add(telemetry.Standard, telemetry.Logs, logCount)
	metrics.Add(telemetry.Standard, telemetry.DirtyPools, uint64(len(dirty)))
	if update.reset == nil {
		r.topology.ObserveTransactions(update.block.Ref, update.transactions)
		r.recordTransferTaxes(update.block.Ref, update.transactions)
	}
	r.block = update.block
	if r.reactive != nil {
		r.reactive.head.Store(r.block.Ref.Number)
	}
	r.silence.canonicalAt = time.Now()
	r.feeParams = update.feeParams
	metrics.Head(telemetry.Standard, r.block.Ref.Number, r.block.Ref.Timestamp)
	if update.tip >= r.block.Ref.Number {
		metrics.Set(telemetry.Standard, telemetry.SyncLag, int64(update.tip-r.block.Ref.Number))
	}
	metrics.Set(telemetry.Standard, telemetry.PoolCount, int64(r.store.Len()))
	if update.reads != nil {
		if r.canonicalReader != nil {
			r.canonicalReader.Close()
		}
		r.canonicalReader = &snapshot{client: r.client, block: r.block, chain: domain.ChainIdentity{Family: r.cfg.Network.Family, ChainID: r.cfg.ChainID()}, deployments: r.cfg.Network.DEX}
		r.canonicalReader.reads.Store(update.reads)
	}
	r.advance()
	if r.discovery != nil {
		r.discovery.observe(update, r)
		if update.reset != nil && r.worker != nil {
			topology, err := pipeline.BuildTopology(ctx, r.store.Pools(), r.topologyOptions())
			if err != nil {
				return err
			}
			graph := r.search.GraphVersion + 1
			if err := r.worker.ReplaceEngine(topology.Engine, graph); err != nil {
				return err
			}
			r.topology = topology
			r.search.GraphVersion = graph
		}
	}
	waiting := r.flashWaiting
	r.flashWaiting, r.flashWaitingBytes = nil, 0
	if r.cfg.FastURL != "" && update.reset == nil {
		r.retireFlashResults()
		// The rebase retires search tickets: search the block's pools and those
		// of the replayed suffix again, as the flashblocks path does. Every pool
		// only after a conflict, whose discarded tentative state was searched.
		affected, err := r.fast.Reconcile(ctx, r.store, update.transactions)
		changed := append(slices.Clone(dirty), affected...)
		if err != nil {
			// A conflict means the tentative state differed from the block
			// the node wrote: the stream cannot be trusted for that block.
			r.metrics.Add(telemetry.Fast, telemetry.ReconcileErrors, 1)
			slog.Warn("fast view does not match the canonical block", "block", update.block.Ref.Number, "error", err)
			if err := r.fast.ResetTo(r.store); err != nil {
				return err
			}
			changed = changed[:0]
			for _, p := range r.store.Pools() {
				changed = append(changed, p.Key)
			}
		}
		if view, version, _, ok := r.fast.View(); ok {
			r.tentative = version
			slices.SortFunc(changed, domain.ComparePoolKeys)
			if err := r.submitSearch(ctx, view, slices.Compact(changed), version); err != nil {
				return err
			}
		}
	} else if r.flashLogs != nil && update.reset == nil {
		if err := r.reconcileFlashLogs(ctx, update, dirty); err != nil {
			return err
		}
	} else if err := r.resetTentative(); err != nil {
		return err
	}
	if r.cfg.JSONOutput {
		if err := json.NewEncoder(os.Stdout).Encode(struct {
			Block domain.BlockRef `json:"block"`
			Reset bool            `json:"reset"`
		}{r.block.Ref, update.reset != nil}); err != nil {
			return err
		}
	}
	for _, block := range waiting {
		r.processingRevision = block.revision
		if err := r.acceptFlashblock(ctx, block.frame, block.receivedAt); err != nil {
			return err
		}
	}
	if r.worker == nil {
		return nil
	}
	// Historical dependency reads compete with the RPC follower for its rate limit.
	// Catch up state first, then rescreen the known universe once at the live tip.
	if update.catchingUp && r.cfg.Blocks == 0 {
		r.catchingUp = true
		if r.catchUp == nil {
			r.catchUp = make(map[domain.PoolKey]struct{})
		}
		for _, key := range r.worker.Invalidate(domain.SourceStandard) {
			r.catchUp[key] = struct{}{}
		}
		for _, key := range dirty {
			r.catchUp[key] = struct{}{}
		}
		r.search.StandardPending = domain.StateVersion{}
		// A canonical reader can stay one block behind a moving RPC tip forever.
		// Its verified state is suitable for bounded background maintenance; only
		// long historical catch-up must defer those reads. Historical search still
		// stays suspended and never competes with the live prefix lane.
		r.discovery.schedule(r)
		if r.ticks != nil {
			r.ticks.schedule(ctx, r)
		}
		return nil
	}
	// At the tip, search what catch-up skipped or dropped. Every pool only after
	// a reset or at the first tip block: rescreening all of them after each
	// one-block lag would rank the complete table several times a minute and
	// hold up the frames behind it.
	if r.rescreen {
		r.rescreen, r.catchingUp, r.catchUp = false, false, nil
		for _, p := range r.store.Pools() {
			dirty = append(dirty, p.Key)
		}
	} else if r.catchingUp {
		r.catchingUp = false
		for key := range r.catchUp {
			dirty = append(dirty, key)
		}
		r.catchUp = nil
	}
	r.discovery.schedule(r)
	if r.ticks != nil {
		r.ticks.schedule(ctx, r)
	}
	if len(update.fees) == 0 {
		// Backends without a fee observation must keep conservative
		// rescreening. When the follower ran its schedule, a scheduled fee
		// that was not due is known unchanged (feeSchedule).
		for _, key := range r.topology.DynamicPools {
			if update.feesScheduled && scheduledFeeKind(key.Kind, key.Hooks) {
				continue
			}
			dirty = append(dirty, key)
		}
	}
	// Reactive drops standard results while a preconfirmation stream is live
	// (standard_while_fast_active), and the rebased stream view was searched
	// above. A canonical rescreen would only hold the next frame's search
	// behind it, so it runs only when the stream is detached.
	if r.streamActive(time.Now()) {
		return nil
	}
	slices.SortFunc(dirty, domain.ComparePoolKeys)
	dirty = slices.Compact(dirty)
	return r.submit(ctx, r.store.View(), dirty, r.version)
}

func (r *runtime) resetTentative() error {
	r.invalidateReactiveObservation("stream_reset", 0)
	r.metrics.Add(telemetry.Fast, telemetry.StreamInvalidations, 1)
	r.invalidateTentative()
	if r.flashLogs != nil {
		if err := r.flashLogs.ResetTo(r.store); err != nil {
			return err
		}
	}
	if r.fast != nil {
		return r.fast.ResetTo(r.store)
	}
	return nil
}

func (r *runtime) invalidateTentative() {
	r.reactiveRevision.Add(1)
	r.tentative = domain.StateVersion{}
	r.flash = flashCursor{}
	r.flashPrefix = flashPrefix{}
	r.flashStart = domain.Hash{}
	r.fastReceivedAt = time.Time{}
	r.deferred = deferredFrames{}
	if r.flashConfirmation != nil {
		r.flashConfirmation.Invalidate()
	}
	r.flashWaiting, r.flashWaitingBytes = nil, 0
	if r.worker != nil {
		r.worker.Invalidate(domain.SourceFast)
		r.search.TentativePending = domain.StateVersion{}
	}
}

func sendReset(ctx context.Context, out chan<- streamEvent) error {
	accepted := make(chan struct{})
	select {
	case out <- streamEvent{reset: true, accepted: accepted}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-accepted:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *runtime) flush(ctx context.Context, searches <-chan arb.SearchEvent, confirmations <-chan pipeline.ConfirmationEvent) error {
	for r.worker != nil && r.search.StandardPending != (domain.StateVersion{}) || r.planPending {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-searches:
			if err := r.search.Process(ctx, event, r, nil, nil, r); err != nil {
				return err
			}
		case event := <-confirmations:
			if err := r.consumeConfirmation(ctx, event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runtime) writeSnapshot() error {
	if r.cfg.PoolSnapshotOut == "" || r.snapshotWritten {
		return nil
	}
	err := writePoolSnapshot(r.cfg.PoolSnapshotOut, r.store)
	r.snapshotWritten = err == nil
	return err
}

// persistOnShutdown writes the snapshot when ctx has ended, whichever path
// returned. Other failures keep their result and write nothing.
func (r *runtime) persistOnShutdown(ctx context.Context, result error) error {
	if ctx.Err() == nil {
		return result
	}
	return errors.Join(result, r.writeSnapshot())
}

// fastQuiet is how long the Arbitrum Fast Feed may go without a message
// before standard blocks are searched again. The chain carries transactions
// every block, so a quiet feed is a detached one.
const fastQuiet = 2 * time.Second

// streamActive reports whether a preconfirmation stream leads the canonical
// view: an attached flashblock stream, or a Fast Feed heard from recently.
func (r *runtime) streamActive(now time.Time) bool {
	return r.flash.active || r.cfg.FastURL != "" && !r.fastReceivedAt.IsZero() && now.Sub(r.fastReceivedAt) < fastQuiet
}
