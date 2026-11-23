package remote

import (
	"context"
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func (r *runtime) acceptFastMessage(ctx context.Context, message feed.FastMessage, receivedAt time.Time, revision uint64) error {
	dequeued := time.Now()
	metrics := telemetry.FromContext(ctx)
	if at, ok := r.feedArrivals.arrivalAt(message.BlockNumber); ok {
		metrics.Observe(telemetry.Fast, telemetry.FeedToFast, receivedAt.Sub(at))
	}
	started := metrics.Start()
	r.frameSteps = frameSteps{}
	step := time.Now()
	lap := func(d *int64) {
		now := time.Now()
		*d, step = now.Sub(step).Nanoseconds(), now
	}
	previous, _ := r.fast.Version()
	dirty, err := r.fast.Apply(ctx, message)
	lap(&r.frameSteps.Apply)
	if err != nil {
		slog.Warn("fast message does not extend the tentative view", "block", message.BlockNumber, "index", message.TxIndex, "error", err)
		return r.resetTentative()
	}
	if r.reactive != nil && r.reactive.quiet != nil && message.Receipt.Successful && len(message.Receipt.Logs) != 0 {
		// A pool these logs change carries no condition until it has rested.
		logs := make([]ingest.Log, len(message.Receipt.Logs))
		for i, log := range message.Receipt.Logs {
			logs[i] = ingest.Log{Address: log.Address, Topics: log.Topics, Data: log.Data}
		}
		r.reactive.quiet.observeFast(message.BlockNumber, logs)
	}
	if r.reactive != nil {
		block := r.frameBlock(message.BlockNumber)
		for _, key := range dirty {
			block.dirty[key] = struct{}{}
		}
	}
	index := uint64(r.cfg.FastCanonicalFirstIndex) + (message.TxIndex - uint64(r.cfg.FastFirstIndex))
	if r.reactive != nil {
		r.reactive.own.changed(dirty, message.BlockNumber, index)
		if r.reactive.competition != nil && len(dirty) >= 2 {
			r.reactive.competition.observeFast(message, index, dirty, receivedAt)
		}
	}
	lap(&r.frameSteps.Observe)
	view, version, _, ok := r.fast.View()
	lap(&r.frameSteps.View)
	if !ok {
		return nil
	}
	if version != previous {
		r.fastReceivedAt, r.fastDequeuedAt = receivedAt, dequeued
		if !receivedAt.IsZero() && !started.IsZero() {
			metrics.Observe(telemetry.Fast, telemetry.FeedQueue, started.Sub(receivedAt))
		}
		metrics.Since(telemetry.Fast, telemetry.PoolApply, started)
		metrics.Since(telemetry.Fast, telemetry.ReceiptLatency, receivedAt)
		metrics.Add(telemetry.Fast, telemetry.Messages, 1)
		metrics.Set(telemetry.Fast, telemetry.HeadBlock, int64(version.BlockNumber))
		metrics.Add(telemetry.Fast, telemetry.Receipts, 1)
		metrics.Add(telemetry.Fast, telemetry.Logs, uint64(len(message.Receipt.Logs)))
		metrics.Add(telemetry.Fast, telemetry.DirtyPools, uint64(len(dirty)))
	}
	r.research.ObservePrefix(pipeline.PrefixObservation{Stream: "arbitrum_fast", Version: version, FrameID: message.TxHash, FrameIndex: message.PGARound, FirstTxIndex: index, Transactions: []domain.Hash{message.TxHash}, TransactionCount: 1, DirtyPools: len(dirty), ReceivedAtNS: receivedAt.UnixNano(), AppliedAtNS: time.Now().UnixNano()})
	r.observeReactiveExecution(message.TxHash, message.Receipt.Successful, message.Receipt.Logs, execution.ExecutionPosition{Stream: "arbitrum_fast", BlockNumber: message.BlockNumber, FrameIndex: message.PGARound, TransactionIndex: index, ReceivedAtNS: receivedAt.UnixNano()})
	lap(&r.frameSteps.Research)
	r.tentative = version
	r.appliedRevision = revision
	return r.submit(ctx, view, dirty, version)
}
