package remote

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func (r *runtime) Current(version domain.StateVersion) bool {
	if version.Source == domain.SourceFast {
		return version == r.tentative && r.appliedRevision == r.inputRevision.Load()
	}
	return version == r.version
}

// deferredFrames holds the pools of Fast frames whose search waited for a newer
// queued frame, and the earliest such frame's arrival. fast is the subset the
// fast lane still owes a quote.
type deferredFrames struct {
	dirty      map[domain.PoolKey]struct{}
	fast       map[domain.PoolKey]struct{}
	receivedAt time.Time
}

func (d *deferredFrames) add(dirty []domain.PoolKey, quoteFast bool, receivedAt time.Time) {
	if d.dirty == nil {
		d.dirty = make(map[domain.PoolKey]struct{})
		d.receivedAt = receivedAt
	}
	for _, key := range dirty {
		d.dirty[key] = struct{}{}
	}
	if !quoteFast {
		return
	}
	if d.fast == nil {
		d.fast = make(map[domain.PoolKey]struct{})
	}
	for _, key := range dirty {
		d.fast[key] = struct{}{}
	}
}

// submit queues a frame for the full search and quotes its changed pools in the
// fast lane.
func (r *runtime) submit(ctx context.Context, view poolstate.View, dirty []domain.PoolKey, version domain.StateVersion) error {
	receivedAt := r.fastReceivedAt
	if version.Source != domain.SourceFast || receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	return r.submitFrame(ctx, view, dirty, version, true, receivedAt)
}

// submitSearch queues a view for the full search only. A reconciled canonical
// block re-searches its pools at the new version; the fast lane already quoted
// them as their frames arrived, and quoting a canonical block's pools on the
// main loop held up the next block's frames. The block triggers this search,
// so it is dated now: the last Fast frame's arrival aged its offers by up to a
// round.
func (r *runtime) submitSearch(ctx context.Context, view poolstate.View, dirty []domain.PoolKey, version domain.StateVersion) error {
	return r.submitFrame(ctx, view, dirty, version, false, time.Now())
}

func (r *runtime) submitFrame(ctx context.Context, view poolstate.View, dirty []domain.PoolKey, version domain.StateVersion, quoteFast bool, receivedAt time.Time) error {
	if r.worker == nil {
		return nil
	}
	var quoteView arb.View = view
	if len(r.waitingFees) > 0 {
		blocked := make(map[domain.PoolKey]bool, len(r.waitingFees))
		for key := range r.waitingFees {
			blocked[key] = true
		}
		quoteView = &dependencyView{View: view, rejected: blocked}
	}
	step := time.Now()
	lap := func(d *int64) {
		now := time.Now()
		*d, step = now.Sub(step).Nanoseconds(), now
	}
	r.research.Advance(pipeline.ResearchFrame{View: quoteView, Version: version, Graph: r.search.GraphVersion, At: time.Now(), Quote: r.topology.Engine.QuoteCycle})
	lap(&r.frameSteps.Advance)
	r.captureExecutionPaper(version, receivedAt)
	lap(&r.frameSteps.Paper)
	// A newer frame is already queued: a quote of this prefix would not be
	// current (Current) and everything it produced would be dropped, while its
	// search delays the frames behind it. Its pools join the newer frame's search.
	if version.Source == domain.SourceFast && r.inputRevision.Load() != r.appliedRevision {
		r.deferred.add(dirty, quoteFast, receivedAt)
		return nil
	}
	var fastDirty []domain.PoolKey
	if quoteFast {
		fastDirty = dirty
	}
	if r.deferred.dirty != nil {
		r.deferred.add(dirty, quoteFast, receivedAt)
		dirty = slices.SortedFunc(maps.Keys(r.deferred.dirty), domain.ComparePoolKeys)
		fastDirty = slices.SortedFunc(maps.Keys(r.deferred.fast), domain.ComparePoolKeys)
		receivedAt = r.deferred.receivedAt
		r.deferred = deferredFrames{}
	}
	if r.reactive != nil {
		r.reactive.touchPools(dirty)
		r.reactive.applied.Store(r.appliedRevision)
		r.considerReactiveCancellation(ctx, quoteView, dirty, version)
	}
	lap(&r.frameSteps.Cancel)
	job := arb.SearchJob{Incremental: true, View: quoteView, Dirty: dirty, Version: version, GraphVersion: r.search.GraphVersion, ReceivedAt: receivedAt}
	if r.cfg.RPCSearchRefresh == "logs" {
		job.QuoteBasis = "log_maintained_pool_state"
	}
	if version.Source == domain.SourceStandard && r.cfg.RPCSearchRefresh != "logs" {
		reader := r.reader()
		job.Prepare = prepareDependencies(reader, r.topology.Table, r.policy.WordRadius, false)
		job.PrepareExact = prepareDependencies(reader, r.topology.Table, r.policy.WordRadius, true)
		job.Release = reader.Close
	}
	// The full search starts on its worker first; the fast lane then runs here
	// in parallel instead of delaying it by its own quoting time.
	err := r.search.Submit(job, nil)
	lap(&r.frameSteps.Submit)
	if r.reactive != nil {
		r.fastLane(ctx, quoteView, fastDirty, version, receivedAt)
	}
	return err
}

func (r *runtime) Consume(ctx context.Context, event arb.SearchEvent) error {
	if r.ticks != nil {
		for _, issue := range event.Result.Issues {
			r.ticks.request(r, issue)
		}
		r.ticks.schedule(ctx, r)
	}
	// Reactive admission goes first: the build runs on its own worker, and
	// research and the candidate journal must not delay it.
	if err := r.queueReactive(event); err != nil {
		return err
	}
	// Routes the full search found profitable rank in the fast lane's next
	// frames too, including those past its per-pool cap (MarkHot). Marked
	// after admission, so the fast-lane coverage of this result describes the
	// frame that produced it rather than this discovery.
	if r.topology != nil && r.topology.Fast != nil {
		now := time.Now()
		for _, o := range event.Result.Opportunities {
			if o.Net != nil && o.Net.Sign() > 0 {
				r.topology.Fast.MarkHot(o.Cycle, now)
			}
		}
	}
	economics := r.research.Observe(event)
	if err := r.retainDependencies(event); err != nil {
		return err
	}
	// Without a candidate file there is nothing to encode: encoding every
	// candidate into a discarded stream can take a fifth of the main loop's
	// CPU.
	if r.candidates != nil {
		if err := (pipeline.CandidateWriter{Economics: economics, Encoder: r.candidates}).Consume(ctx, event); err != nil {
			return err
		}
	}
	if err := r.offerExecutionPaper(event); err != nil {
		return err
	}
	if r.confirmation == nil || event.Stage == arb.QuoteInitial {
		return nil
	}
	opportunities, _ := pipeline.SelectCandidates(event.Result.Opportunities, r.execution.MaxCandidates, func(asset domain.Address) bool { _, ok := r.execution.Assets[asset]; return ok })
	if len(opportunities) == 0 {
		return nil
	}
	if event.Job.Version.Source == domain.SourceFast {
		return r.confirmFlashCandidates(event, opportunities)
	}
	if event.Job.Version.Source != domain.SourceStandard {
		return nil
	}
	batch := planning.Batch{Version: event.Job.Version, Graph: event.Job.GraphVersion, View: event.Job.View, Snapshot: r.reader(), Opportunities: opportunities}
	task := &confirmationTask{batch: batch, policy: r.execution}
	job := pipeline.ConfirmationJob{Identity: pipeline.ConfirmationIdentity{Version: event.Job.Version, Graph: event.Job.GraphVersion, ReceivedAt: event.Job.ReceivedAt}, Task: task}
	if event.Partial {
		accepted, err := r.confirmation.Offer(job, r.execution.MaxCandidates)
		if !accepted {
			task.Close()
		}
		if err != nil || !accepted {
			return err
		}
	} else if err := r.confirmation.Submit(job); err != nil {
		task.Close()
		return err
	}
	r.planPending = true
	return nil
}

type confirmationTask struct {
	batch  planning.Batch
	policy planning.Policy
}

func (t *confirmationTask) Run(ctx context.Context) (*execution.ReadyPlan, []string, error) {
	return planning.Build(ctx, t.batch, t.policy, time.Now)
}
func (t *confirmationTask) Close() { t.batch.Snapshot.Close() }

func (r *runtime) consumeConfirmation(ctx context.Context, event pipeline.ConfirmationEvent) error {
	if !r.confirmation.Current(event) || !r.Current(event.Identity.Version) {
		return nil
	}
	r.planPending = r.confirmation.PendingAfter(event)
	r.research.Confirmation(event)
	if event.Err != nil {
		slog.Warn("remote candidate confirmation failed", "error", event.Err)
		return nil
	}
	if event.Plan == nil {
		return nil
	}
	if err := execution.ValidateForDispatch(ctx, event.Plan, r.version, r.cfg.ChainID(), r.search.GraphVersion, r.execution.PolicyHash, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("remote plan validation: %w", err)
	}
	if r.discovery != nil {
		r.discovery.activity.Profitable(pipeline.ProfitablePlanPools(event.Plan))
	}
	return r.deliver(ctx, telemetry.Standard, event.Plan, event.Identity.ReceivedAt)
}

// deliver is the only exit for validated plans, so the gate holds both sources.
func (r *runtime) deliver(ctx context.Context, source telemetry.Source, plan *execution.ReadyPlan, receivedAt time.Time) error {
	if err := execution.CheckRouteTokens(plan.Route, r.risks.Check); err != nil {
		r.metrics.Add(source, telemetry.PlanDiscarded, 1)
		return nil
	}
	metrics := telemetry.FromContext(ctx)
	if r.gate.Paused() {
		pipeline.ObserveHeld(metrics, source, plan)
		return nil
	}
	if err := r.plans.Encode(plan); err != nil {
		return err
	}
	pipeline.ObserveDelivered(metrics, source, plan, receivedAt)
	return nil
}
