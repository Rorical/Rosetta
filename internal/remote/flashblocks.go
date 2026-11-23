package remote

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

type flashCursor struct {
	payload                        string
	index, number, nextTransaction uint64
	digest                         domain.Hash
	active                         bool
}

// The first block attaches to a verified RPC anchor. Once attached, complete
// ordered frames advance the pool branch across blocks without waiting for RPC.
// Subsequent parent hashes are tentative seals checked by background reconciliation.
func (r *runtime) acceptFlashblock(ctx context.Context, block feed.Flashblock, receivedAt time.Time) error {
	encoded, err := json.Marshal(block)
	if err != nil {
		return err
	}
	hash := digest(encoded)
	var dirty []domain.PoolKey
	if block.Number <= r.store.Block().Number {
		return nil
	}
	if len(r.flashWaiting) > 0 {
		return r.waitForFlashParent(block, len(encoded), receivedAt)
	}
	if r.flash.active && r.flash.payload == block.PayloadID && r.flash.index == block.Index {
		if r.flash.digest != hash {
			return r.resetTentative()
		}
		return nil
	}
	if block.Index == 0 {
		if r.flash.active && block.Number == r.flash.number {
			// Replacing a payload can undo effects absent from the replacement.
			r.invalidateReactiveObservation("payload_replaced", block.Number)
			dirty = r.flashLogs.AffectedPools()
		}
		if err := r.beginFlashLogs(ctx, block, len(encoded), receivedAt); err != nil {
			return err
		}
		if !r.flash.active {
			return nil
		}
	} else if !r.flash.active || r.flash.payload != block.PayloadID || r.flash.number != block.Number || block.Index != r.flash.index+1 {
		return r.resetTentative()
	}
	metrics := telemetry.FromContext(ctx)
	metrics.Since(telemetry.Fast, telemetry.FeedQueue, receivedAt)
	started := metrics.Start()
	if err := r.flashPrefix.append(block, len(encoded)); err != nil {
		return r.resetTentative()
	}
	if r.flashConfirmation != nil {
		r.flashConfirmation.Invalidate()
	}
	firstTxIndex := r.flash.nextTransaction
	var baseFee *big.Int
	if execution := r.flashPrefix.Base.Execution; execution != nil {
		baseFee = execution.BaseFee
	}
	hashes := make([]domain.Hash, 0, len(block.Transactions))
	var logCount uint64
	for _, transaction := range block.Transactions {
		hashes = append(hashes, transaction.Hash)
		tx := ingest.Transaction{Hash: transaction.Hash, Index: r.flash.nextTransaction, Successful: transaction.Successful}
		for _, log := range transaction.Logs {
			tx.Logs = append(tx.Logs, ingest.Log{Address: log.Address, Topics: log.Topics, Data: log.Data})
		}
		changed, err := r.flashLogs.Apply(ctx, tx)
		if err != nil {
			return errors.Join(err, r.resetTentative())
		}
		dirty = append(dirty, changed...)
		if r.reactive != nil {
			r.reactive.own.changed(changed, block.Number, r.flash.nextTransaction)
		}
		if r.reactive != nil && r.reactive.competition != nil && len(changed) >= 2 {
			r.reactive.competition.observeFlash(block, transaction, r.flash.nextTransaction, changed, baseFee, receivedAt)
		}
		logCount += uint64(len(transaction.Logs))
		r.flash.nextTransaction++
	}
	for index, transaction := range block.Transactions {
		r.observeReactiveExecution(transaction.Hash, transaction.Successful, transaction.Logs, execution.ExecutionPosition{Stream: "flashblocks", PayloadID: block.PayloadID, BlockNumber: block.Number, FrameIndex: block.Index, TransactionIndex: firstTxIndex + uint64(index), ReceivedAtNS: receivedAt.UnixNano()})
	}
	r.flash.index, r.flash.digest = block.Index, hash
	slices.SortFunc(dirty, domain.ComparePoolKeys)
	dirty = slices.Compact(dirty)
	// Empty frames still establish stream activity before any receipt opens a view.
	metrics.Add(telemetry.Fast, telemetry.Messages, 1)
	metrics.Set(telemetry.Fast, telemetry.HeadBlock, int64(block.Number))
	if block.Base != nil {
		metrics.Set(telemetry.Fast, telemetry.HeadTimestamp, int64(block.Base.Timestamp))
	}
	metrics.Add(telemetry.Fast, telemetry.Receipts, uint64(len(block.Transactions)))
	metrics.Add(telemetry.Fast, telemetry.Logs, logCount)
	metrics.Add(telemetry.Fast, telemetry.DirtyPools, uint64(len(dirty)))
	// Bind empty initial frames before a receipt opens the pool branch.
	previous := r.flashPrefix.Version.PrefixHash
	prefixHash := digest(append(previous[:], hash[:]...))
	r.flashPrefix.Version.PrefixHash = prefixHash
	view, version, ok := r.flashLogs.View()
	if !ok {
		version = domain.StateVersion{Source: domain.SourceFast, Anchor: r.store.Block(), BlockNumber: block.Number, PrefixHash: prefixHash}
		r.research.ObservePrefix(pipeline.PrefixObservation{Stream: "flashblocks", Transactions: hashes, Version: version, FrameID: hash, FrameIndex: block.Index, FirstTxIndex: firstTxIndex, TransactionCount: uint64(len(block.Transactions)), DirtyPools: len(dirty), ReceivedAtNS: receivedAt.UnixNano(), AppliedAtNS: time.Now().UnixNano()})
		return nil
	}
	// Even an empty heartbeat advances the source position and retires results
	// from an earlier subblock. The digest binds payload, index, txs and logs.
	version.PrefixHash = prefixHash
	r.flashPrefix.Version = version
	r.research.ObservePrefix(pipeline.PrefixObservation{Stream: "flashblocks", Transactions: hashes, Version: version, FrameID: hash, FrameIndex: block.Index, FirstTxIndex: firstTxIndex, TransactionCount: uint64(len(block.Transactions)), DirtyPools: len(dirty), ReceivedAtNS: receivedAt.UnixNano(), AppliedAtNS: time.Now().UnixNano()})
	r.flashPrefix.Source = flashSource(r.cfg.RPCURL, r.cfg.FlashblocksURL)
	metrics.Since(telemetry.Fast, telemetry.PoolApply, started)
	metrics.Since(telemetry.Fast, telemetry.ReceiptLatency, receivedAt)
	r.fastReceivedAt = receivedAt
	r.appliedRevision = r.processingRevision
	r.tentative = version
	return r.submit(ctx, view, dirty, version)
}
