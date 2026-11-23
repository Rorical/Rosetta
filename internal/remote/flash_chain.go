package remote

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

type pendingFlashblock struct {
	revision   uint64
	frame      feed.Flashblock
	receivedAt time.Time
}

func (r *runtime) beginFlashLogs(ctx context.Context, block feed.Flashblock, size int, receivedAt time.Time) error {
	if block.Base == nil {
		return r.resetTentative()
	}
	start := r.flashPrefix.Version.PrefixHash
	if r.flash.active && block.Number == r.flash.number {
		start = r.flashStart
		r.retireFlashResults()
		if err := r.flashLogs.Rewind(ctx, block.Number); err != nil {
			return errors.Join(err, r.resetTentative())
		}
	} else if r.flash.active && block.Number != r.flash.number+1 {
		if err := r.resetTentative(); err != nil {
			return err
		}
		start = domain.Hash{}
	}
	if r.flashLogs == nil {
		branch, err := ingest.NewLogBranch(r.store)
		if err != nil {
			return err
		}
		r.flashLogs = branch
	}
	ref := domain.BlockRef{Number: block.Number, ParentHash: block.Base.Parent, Timestamp: block.Base.Timestamp}
	if err := r.flashLogs.Begin(ctx, ref); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if resetErr := r.resetTentative(); resetErr != nil {
			return resetErr
		}
		if block.Number > r.store.Block().Number+1 {
			// Joining after a gap still requires the missing base. This queue is not
			// used for ordinary block transitions on an already-attached branch.
			return r.waitForFlashParent(block, size, receivedAt)
		}
		return nil
	}
	r.flash = flashCursor{payload: block.PayloadID, number: block.Number, active: true}
	r.flashStart = start
	r.flashPrefix = flashPrefix{Version: domain.StateVersion{PrefixHash: start}}
	return nil
}

func (r *runtime) reconcileFlashLogs(ctx context.Context, update canonicalUpdate, dirty []domain.PoolKey) error {
	r.retireFlashResults()
	matched, err := r.flashLogs.Reconcile(ctx, r.store, update.transactions)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		telemetry.FromContext(ctx).Add(telemetry.Fast, telemetry.ReconcileErrors, 1)
		slog.Warn("stream pool suffix detached during canonical reconciliation", "block", update.block.Ref.Number, "error", err)
		return r.resetTentative()
	}
	if !matched {
		telemetry.FromContext(ctx).Add(telemetry.Fast, telemetry.ReconcileErrors, 1)
		slog.Warn("stream pool receipt tail repaired from canonical state", "block", update.block.Ref.Number)
	}
	view, version, ok := r.flashLogs.View()
	if !ok {
		r.flash = flashCursor{}
		r.flashPrefix = flashPrefix{}
		r.flashStart = domain.Hash{}
		return nil
	}
	version.PrefixHash = r.flashPrefix.Version.PrefixHash
	r.flashPrefix.Version = version
	r.tentative = version
	dirty = append(dirty, r.flashLogs.AffectedPools()...)
	return r.submit(ctx, view, dirty, version)
}

// Waiting is reserved for initial attachment or repair after a discontinuity.
// Preserve multiple contiguous payloads so a slow RPC can attach an older one.
func (r *runtime) waitForFlashParent(block feed.Flashblock, size int, receivedAt time.Time) error {
	if len(r.flashWaiting) > 0 {
		last := r.flashWaiting[len(r.flashWaiting)-1].frame
		if block.Number == last.Number && block.PayloadID == last.PayloadID && block.Index == last.Index {
			encoded, err := json.Marshal(block)
			if err != nil {
				return err
			}
			previous, _ := json.Marshal(last)
			if digest(encoded) == digest(previous) {
				return nil
			}
			return r.resetTentative()
		}
		continuous := block.Number == last.Number && block.PayloadID == last.PayloadID && block.Index == last.Index+1
		nextBlock := last.Number != ^uint64(0) && block.Number == last.Number+1 && block.Index == 0 && block.Base != nil
		if !continuous && !nextBlock {
			if err := r.resetTentative(); err != nil {
				return err
			}
			if block.Index != 0 {
				return nil
			}
		}
	}
	if len(r.flashWaiting) >= 256 || size > 32<<20-r.flashWaitingBytes {
		return r.resetTentative()
	}
	r.flashWaiting = append(r.flashWaiting, pendingFlashblock{frame: block, receivedAt: receivedAt, revision: r.processingRevision})
	r.flashWaitingBytes += size
	return nil
}
