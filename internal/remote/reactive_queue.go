package remote

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// offerJob keeps up to one job per lane, queued or running. A job
// competes with the active jobs whose routes it shares, or, when every lane
// is spoken for, with the worst one; it replaces them only if it is better,
// and a replaced job that is already running fails its final check. With one
// lane this is the earlier single slot: a better quote replaces the one
// waiting or signing.
func (s *reactiveRuntime) offerJob(job reactiveJob) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	now := time.Now().UnixMilli()
	var active, conflicts []*reactiveJob
	for _, q := range s.slots {
		if q.current() && q.expiresMS() > now {
			active = append(active, q)
			if overlaps(routePools(job.intent), routePools(q.intent)) && (job.group == 0 || job.group != q.group) {
				conflicts = append(conflicts, q)
			}
		}
	}
	// A variant is a hedge: it takes only an idle lane and replaces nothing.
	if job.variant && (len(conflicts) > 0 || len(active) > len(s.extra)) {
		return
	}
	if len(conflicts) == 0 && len(active) > len(s.extra) {
		conflicts = []*reactiveJob{worstJob(active)}
	}
	for _, q := range conflicts {
		if !preferJob(&job, q) {
			return
		}
	}
	for _, q := range conflicts {
		q.superseded.Store(true)
	}
	// Waiting jobs that were replaced or went stale never run.
	kept := s.slots[:0]
	for _, q := range s.slots {
		if q.taken || !q.superseded.Load() && q.current() && q.expiresMS() > now {
			kept = append(kept, q)
		}
	}
	superseded, base := new(atomic.Bool), job.current
	job.superseded = superseded
	job.current = func() bool { return !superseded.Load() && base() }
	s.slots = append(kept, &job)
	for _, lane := range s.allLanes() {
		select {
		case lane.notify <- struct{}{}:
		default:
		}
	}
}

// preferJob is the replacement rule: an opportunity beats any variant, a
// cancellation never replaces an arbitrage, a cancellation is replaced by any
// arbitrage, and arbitrages compare by expected net.
func preferJob(next, prior *reactiveJob) bool {
	if next.variant != prior.variant {
		return prior.variant
	}
	if next.cancellation != nil && prior.cancellation == nil {
		return false
	}
	if next.cancellation == nil && prior.cancellation == nil {
		return execution.BetterReactive(next.intent, prior.intent, 0)
	}
	return true
}

// worstJob is the job a new one must beat for a lane.
func worstJob(jobs []*reactiveJob) *reactiveJob {
	worst := jobs[0]
	for _, q := range jobs[1:] {
		if preferJob(worst, q) {
			worst = q
		}
	}
	return worst
}

func (j *reactiveJob) expiresMS() int64 {
	if j.cancellation != nil {
		return j.cancellation.ExpiresMS
	}
	return j.intent.ExpiresMS
}

// takeJob hands lane its best waiting job, or nil.
func (s *reactiveRuntime) takeJob(lane *sendLane) *reactiveJob {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if !lane.available() {
		return nil
	}
	var pick *reactiveJob
	for _, q := range s.slots {
		if !q.taken && lane.eligible(q) && (pick == nil || preferJob(q, pick)) {
			pick = q
		}
	}
	if pick != nil {
		pick.taken = true
	}
	return pick
}

func (s *reactiveRuntime) finishJob(job *reactiveJob) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	s.slots = slices.DeleteFunc(s.slots, func(q *reactiveJob) bool { return q == job })
}

// executeJob runs job on lane. Only a single primary lane replaces a pending
// transaction; with extra lanes a new intent takes a free lane instead.
func (s *reactiveRuntime) executeJob(ctx context.Context, job *reactiveJob, lane *sendLane) map[string]any {
	d := lane.dispatcher
	action := "submit"
	if job.cancellation != nil {
		action = "cancel"
	} else if len(s.extra) == 0 && d != nil && d.Pending() != nil && !d.Included() {
		action = "replace"
	}
	timing := job.timing
	timing.ExecuteNS = time.Now().UnixNano()
	// The slots read job.intent under queueMu; the lane's copy is local.
	intent := job.intent
	result := map[string]any{"type": "reactive_intent", "mode": s.settings.document.Mode, "action": action, "intent": intent, "timing": timing}
	if action != "cancel" && lane.index != 0 {
		resealed, err := execution.ForSender(ctx, intent, lane.sender)
		if err != nil {
			result["status"], result["error"] = "lane_reseal_failed", err.Error()
			return result
		}
		intent = resealed
		result["intent"], result["lane"] = intent, lane.index
	}
	if d == nil {
		if !job.current() || intent.Validate(ctx, time.Now()) != nil {
			result["status"] = "stale"
		} else if status, ok := s.preflightStatus(ctx, intent, result); !ok {
			result["status"] = status
		} else {
			result["status"] = "shadow_admitted"
			if job.verify != nil {
				s.offerVerification(job.verify)
			}
		}
		return result
	}
	// A paused gate stops anything that adds exposure. Cancellation only removes
	// it, so it must keep working during exactly the incident that caused a pause.
	current := job.current
	if action != "cancel" {
		if s.gate.Paused() {
			s.metrics.Add(pipeline.MetricSource(intent.Version.Source), telemetry.PlansHeld, 1)
			result["status"] = "held"
			return result
		}
		if s.backlog.held(time.Now()) {
			result["status"] = "sequencer_backlog"
			return result
		}
		// After a start without saved word models a route's pools would go
		// out unconditioned, and a lost race would be paid for.
		if s.quiet != nil && !s.quiet.ready() {
			result["status"] = "conditions_warming"
			return result
		}
		// A job built to replace a pending transaction is sent as a new nonce
		// once that transaction lands, and then runs after it: its quote must
		// already include it. moved checks that, and any other send of ours.
		var replaced *execution.PendingTransaction
		if action == "replace" {
			replaced = d.Pending()
		}
		pools := routePools(intent)
		unmoved := func() bool { return !overlaps(pools, s.own.moved(intent.Version, replaced, job.group, time.Now())) }
		unheld := func() bool { return !overlaps(pools, s.own.held(time.Now())) }
		if !unmoved() {
			result["status"] = "own_tx_not_in_quote"
			return result
		}
		if !unheld() {
			result["status"] = "own_revert_unexplained"
			return result
		}
		// The gate can close while this job waits to sign, so re-check at the send.
		current = func() bool {
			return s.failure.Load() == nil && !s.gate.Paused() && job.current() && unmoved() && unheld()
		}
	}
	if job.target != (domain.Hash{}) {
		pending := d.Pending()
		if pending == nil || pending.Hash != job.target {
			result["status"] = "target_changed"
			return result
		}
	}
	if action != "cancel" {
		if status, ok := s.preflightStatus(ctx, intent, result); !ok {
			result["status"] = status
			return result
		}
		// Another lane may be sending through these pools right now; own.sent
		// learns of it only once that send returns.
		if !s.own.reserve(lane.index, routePools(intent), job.group) {
			result["status"] = "own_tx_in_flight"
			return result
		}
		defer s.own.release(lane.index)
	}
	var hash domain.Hash
	var err error
	switch action {
	case "cancel":
		hash, err = d.Cancel(ctx, *job.cancellation, current)
	case "replace":
		hash, err = d.Replace(ctx, intent, current)
	default:
		hash, err = d.Dispatch(ctx, intent, current)
	}
	result["transaction"] = hash
	result["submission"] = d.Submission(hash)
	if pending := d.Pending(); action != "cancel" && hash != (domain.Hash{}) && pending != nil && pending.Hash == hash {
		s.own.sent(lane.index, pending.Nonce, routePools(intent), job.group, time.Now())
	}
	s.observeSend(job, action, hash, lane)
	// A signed route is replayed at its quote state, so a revert can be told
	// apart from a route that would have failed even unchallenged.
	if action != "cancel" && hash != (domain.Hash{}) && job.verify != nil {
		s.offerVerification(job.verify)
	}
	if errors.Is(err, execution.ErrConditionsRejected) {
		if s.bids != nil {
			s.bids.observeFreeLoss()
			// The win model has no receipt to learn this loss from.
			if bid := intent.Bid; bid != nil && bid.Worth != 0 && intent.Call.TipCap != nil {
				s.bids.settle(bidOutcome{pools: routePools(intent), worth: bid.Worth, tip: rivalTip(intent.Call.TipCap), dropped: true, block: intent.Version.BlockNumber, at: time.Now()})
			}
		}
		// The move that failed the condition may not be in the next view yet.
		s.own.rejected(routePools(intent), time.Now())
	}
	if err != nil {
		result["error"] = err.Error()
		if !errors.Is(err, execution.ErrDispatchRejected) && !errors.Is(err, execution.ErrDispatchBusy) && !errors.Is(err, execution.ErrSubmissionUnknown) {
			s.failure.Store(&err)
		}
	}
	return result
}

// Frames bump counters on the main loop; the build worker captures them when it
// creates a guard. Unrelated frames preserve a quote's dependency validity.
func (s *reactiveRuntime) touchPools(keys []domain.PoolKey) {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	for _, key := range keys {
		if counter := s.poolVersions[key]; counter != nil {
			counter.Add(1)
		}
	}
}

// reactiveGuard returns nil when input moved past revision, the input the quote
// was taken from. The check shares poolMu with the counter snapshot: a frame
// bumps inputRevision before touchPools, so an unchanged revision under the lock
// means no newer frame has reached the counters.
func (r *runtime) reactiveGuard(keys []domain.PoolKey, cache *reactiveCache, generation, revision uint64) func() bool {
	s := r.reactive
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if revision != r.inputRevision.Load() || generation != r.reactiveRevision.Load() {
		return nil
	}
	if s.poolVersions == nil {
		s.poolVersions = map[domain.PoolKey]*atomic.Uint64{}
	}
	type dependency struct {
		counter *atomic.Uint64
		version uint64
	}
	dependencies := make([]dependency, 0, len(keys))
	for _, key := range keys {
		counter := s.poolVersions[key]
		if counter == nil {
			counter = &atomic.Uint64{}
			s.poolVersions[key] = counter
		}
		dependencies = append(dependencies, dependency{counter, counter.Load()})
	}
	return func() bool {
		if generation != r.reactiveRevision.Load() || s.applied.Load() != r.inputRevision.Load() || s.cache.Load() != cache || time.Now().UnixMilli() >= cache.costs.ExpiresMS {
			return false
		}
		for _, d := range dependencies {
			if d.counter.Load() != d.version {
				return false
			}
		}
		return true
	}
}

// preflightStatus runs the configured preflight and reports whether the job
// may proceed; otherwise it names the status that drops it.
func (s *reactiveRuntime) preflightStatus(ctx context.Context, intent execution.ReactiveIntent, result map[string]any) (string, bool) {
	if s.preflight == nil {
		return "", true
	}
	started := time.Now()
	skipped, err := s.preflight.check(ctx, intent)
	if skipped {
		result["preflight"] = "route_passed"
		return "", true
	}
	result["preflight_ns"] = time.Since(started).Nanoseconds()
	if err == nil {
		return "", true
	}
	result["error"] = err.Error()
	if errors.Is(err, errPreflightRevert) {
		return "preflight_revert", false
	}
	if errors.Is(err, errPreflightUnproven) {
		if s.preflight.covers(intent) {
			delete(result, "error")
			result["preflight"] = "unproven_covered"
			return "", true
		}
		return "preflight_unproven", false
	}
	return "preflight_unavailable", false
}

// offerVerification keeps only the newest intent; an older one still waiting
// for the paper worker would be replayed against an older quote state.
func (s *reactiveRuntime) offerVerification(job *paperExecutionJob) {
	s.verifyMu.Lock()
	defer s.verifyMu.Unlock()
	if s.verification != nil {
		s.skip("verification_superseded")
	}
	s.verification = job
}
func (s *reactiveRuntime) verificationPending() bool {
	s.verifyMu.Lock()
	defer s.verifyMu.Unlock()
	return s.verification != nil
}
func (s *reactiveRuntime) takeVerification() *paperExecutionJob {
	s.verifyMu.Lock()
	defer s.verifyMu.Unlock()
	job := s.verification
	s.verification = nil
	return job
}
