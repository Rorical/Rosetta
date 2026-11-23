package remote

import (
	"context"
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// fastLaneCPUs is how many cores the full search leaves to the main loop's
// frame handling and fast-lane quote.
const fastLaneCPUs = 1

// fastLane quotes the two- and three-hop routes through a frame's changed pools
// on the main loop and hands any opportunity straight to the reactive build
// worker. It skips the search worker's queue, catalog exploration and wide
// ranking, which cost tens to hundreds of milliseconds per frame on OP. The full
// search still runs; a later better intent replaces the pending one.
func (r *runtime) fastLane(ctx context.Context, view arb.View, dirty []domain.PoolKey, version domain.StateVersion, received time.Time) {
	if r.reactive == nil || !r.reactive.settings.document.FastLane || r.topology == nil || r.topology.Fast == nil || len(dirty) == 0 {
		return
	}
	started := time.Now()
	result, err := r.topology.Fast.EvaluateRanked(ctx, view, dirty, r.searchOptions)
	r.metrics.Since(pipeline.MetricSource(version.Source), telemetry.FastLaneQuote, started)
	if err != nil {
		slog.Warn("fast lane quote failed", "error", err)
		return
	}
	r.fastQuoted.version, r.fastQuoted.routes = version, make(map[string]struct{}, len(result.Opportunities))
	r.fastQuoted.selected = make(map[string]struct{}, len(result.SelectedRoutes))
	for _, key := range result.SelectedRoutes {
		r.fastQuoted.selected[key] = struct{}{}
	}
	block := r.frameBlock(version.BlockNumber)
	for _, o := range result.Opportunities {
		key := arb.RouteKey(o.Cycle)
		r.fastQuoted.routes[key] = struct{}{}
		block.quoted[key] = struct{}{}
	}
	if len(result.Opportunities) == 0 {
		r.reactive.skip("fast_lane_no_opportunity")
		return
	}
	event := arb.SearchEvent{Stage: arb.QuoteOptimized, QuotedAt: time.Now(), Result: result,
		Job: arb.SearchJob{Incremental: true, View: view, Dirty: dirty, Version: version, ReceivedAt: received}}
	var dequeued time.Time
	if version.Source == domain.SourceFast {
		dequeued = r.fastDequeuedAt
	}
	var frame *frameSteps
	if version.Source == domain.SourceFast {
		copied := r.frameSteps
		frame = &copied
	}
	if err := r.queueReactiveLane(event, "fast", dequeued, started, frame); err != nil {
		slog.Warn("fast lane admission failed", "error", err)
	}
}

// frameBlock keeps what the frames of one block did, for blockFrameDepth
// blocks: enough to classify a full-search route found after the block's
// canonical reconcile.
type frameBlock struct {
	dirty  map[domain.PoolKey]struct{}
	quoted map[string]struct{}
}

const blockFrameDepth = 4

func (r *runtime) frameBlock(number uint64) *frameBlock {
	if r.blockFrames == nil {
		r.blockFrames = make(map[uint64]*frameBlock)
	}
	block := r.blockFrames[number]
	if block == nil {
		block = &frameBlock{dirty: make(map[domain.PoolKey]struct{}), quoted: make(map[string]struct{})}
		r.blockFrames[number] = block
		for old := range r.blockFrames {
			if old+blockFrameDepth < number {
				delete(r.blockFrames, old)
			}
		}
	}
	return block
}
