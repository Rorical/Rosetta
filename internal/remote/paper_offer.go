package remote

import (
	"context"
	"errors"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
)

func (r *runtime) offerExecutionPaper(event arb.SearchEvent) error {
	p := r.executionPaper
	if p == nil {
		return nil
	}
	if err := p.error(); err != nil {
		return err
	}
	if len(event.Result.Opportunities) == 0 || event.Stage == arb.QuoteInitial && p.priority.minimum == nil {
		return nil
	}
	now := time.Now()
	p.flushSelection(now, false)
	reason := ""
	if p.window != nil {
		// Continue collecting this bounded paper-only window.
	} else if p.waiting != nil || r.reactive != nil && r.reactive.verificationPending() {
		reason = "reactive_verification_pending"
	} else if now.UnixMilli() < p.backoff.Load() {
		reason = "backoff"
	} else if p.busy.Load() {
		reason = "worker_busy"
	}
	if reason != "" {
		for _, o := range event.Result.Opportunities {
			p.sampler.skip(o, event.Job.Version, now, reason)
		}
		if reason == "worker_busy" {
			p.skipped.Add(1)
		}
		return nil
	}
	wrapped, _ := domain.ParseAddress(p.policy.WrappedNative)
	selected := -1
	var selection *paperSelection
	skipReasons := make(map[int]string)
	var lenders []pool.State
	fundable := func(index int, _ *paperSelection, o arb.Opportunity) bool {
		if err := candidateTokenRisk(event.Job.View, o, p.risks.Check); err != nil {
			skipReasons[index] = "token_transfer_shortfall"
			return false
		}
		if lenders == nil {
			lenders = r.store.Pools()
		}
		choice, err := paperFunding(context.Background(), o, lenders, wrapped)
		if err == nil {
			if reason := r.cachedPaperFee(event, o, choice, now); reason != "" {
				skipReasons[index] = reason
				return false
			}
			return true
		}
		skipReasons[index] = "unsupported_funding"
		if errors.Is(err, planning.ErrFundingUnprofitable) {
			skipReasons[index] = "funding_cost_exceeds_gross"
		}
		return false
	}
	if p.priority.minimum != nil {
		selected, selection = p.sampler.choose(event, p.research, wrapped, "high_gross_priority", func(index int, candidate *paperSelection, o arb.Opportunity) bool {
			if candidate.NativeGross == nil || candidate.NativeGross.Cmp(p.priority.minimum) < 0 {
				return false
			}
			route := paperRouteIdentity(o)
			reason := p.priority.reason(now, route)
			if reason != "" {
				skipReasons[index] = reason
			}
			return reason == "" && fundable(index, candidate, o)
		})
	}
	priority := selected >= 0
	if !priority && event.Stage != arb.QuoteInitial && (p.window != nil || !now.Before(p.next)) {
		selected, selection = p.sampler.chooseCandidate(event, p.research, wrapped, fundable)
	}

	for i, o := range event.Result.Opportunities {
		if i == selected {
			continue
		}
		reason := "stratum_not_selected"
		if selected < 0 {
			reason = "interval"
			if event.Stage == arb.QuoteInitial {
				reason = "initial_quote"
			}
		}
		if specific := skipReasons[i]; specific != "" {
			reason = specific
		}
		if o.Net != nil && o.Net.Sign() > 0 && !paperStratumFor(o).Supported {
			reason = "unsupported_executor_pool"
		}
		p.sampler.skip(o, event.Job.Version, now, reason)
	}
	if selected < 0 {
		return nil
	}
	if p.window != nil && !priority && !p.sampler.prefers(selection, event.Result.Opportunities[selected], p.window.job.selection, p.window.job.opportunity) {
		p.sampler.skip(event.Result.Opportunities[selected], event.Job.Version, now, "selection_window_loser")
		return nil
	}
	if p.window == nil && !p.busy.CompareAndSwap(false, true) {
		return nil
	}
	if p.window == nil && !priority {
		p.next = now.Add(time.Duration(p.policy.IntervalMS) * time.Millisecond)
	}
	job := r.paperStateJob(event, now)
	job.opportunity, job.selection, job.lenders = event.Result.Opportunities[selected], selection, lenders
	if p.window != nil {
		p.sampler.skip(p.window.job.opportunity, p.window.job.event.Job.Version, p.window.job.discovered, "selection_window_replaced")
		p.window.job = job
	} else {
		p.window = &paperSelectionWindow{job: job, until: now.Add(time.Duration(p.policy.SamplingWindowMS) * time.Millisecond)}
	}
	if priority {
		p.priority.admit(now, paperRouteIdentity(job.opportunity))
	}
	p.flushSelection(now, priority)
	return nil
}

// paperStateJob binds a search result to the exact state it was quoted from.
// Main loop only: the live prefix is frozen before the next frame extends it.
func (r *runtime) paperStateJob(event arb.SearchEvent, discovered time.Time) paperExecutionJob {
	job := paperExecutionJob{event: event, discovered: discovered, anchor: r.block}
	job.event.Result = arb.SearchResult{}
	job.event.Job.Release = nil
	if event.Job.Version.Source == domain.SourceFast {
		if r.flashPrefix.Version != event.Job.Version || !r.flash.active {
			job.unavailable = "candidate prefix is no longer retained"
		} else if len(r.flashPrefix.Transactions) > r.executionPaper.policy.MaxPrefixTransactions {
			job.unavailable = "prefix exceeds execution paper replay limit"
		} else {
			prefix := r.flashPrefix.freeze()
			job.prefix = &prefix
		}
	} else if event.Job.Version.Anchor != r.block.Ref {
		job.unavailable = "candidate canonical snapshot is no longer retained"
	}
	return job
}
