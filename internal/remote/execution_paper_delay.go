package remote

import (
	"encoding/json"
	"math/big"
	"reflect"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

const paperCaptureGrace = 500 * time.Millisecond

// paperDelayCapture collects one group's later-prefix scenarios. Only the main
// loop touches it. A group waiting for the worker captures into captured; the
// running group (p.pending) queues each scenario directly. The remaining
// counter keeps admission closed until every scenario is durable.
type paperDelayCapture struct {
	original paperExecutionJob
	next     int
	captured []paperExecutionJob
}

// capture fixes the group identity and quote context of a new group.
func (p *executionPaper) capture(job paperExecutionJob) *paperDelayCapture {
	job.quoteTimestamp = job.anchor.Ref.Timestamp
	job.quoteBaseFee = job.anchor.BaseFee
	if job.prefix != nil {
		job.quoteTimestamp = job.prefix.Base.Timestamp
		if job.prefix.Base.Execution != nil {
			job.quoteBaseFee = job.prefix.Base.Execution.BaseFee
		}
	}
	if job.quoteBaseFee != nil {
		job.quoteBaseFee = new(big.Int).Set(job.quoteBaseFee)
	}
	identity, _ := json.Marshal([]any{p.chain, p.artifact.hash, job.event.Job.Version, job.opportunity.Cycle, job.opportunity.Input, job.discovered.UnixNano()})
	job.group = digest(identity)
	job.executionVersion = job.event.Job.Version
	job.observed = job.event.Job.ReceivedAt
	c := &paperDelayCapture{original: job}
	// Canonical-only samples have no sub-block arrival observations. Do not
	// substitute a later latest RPC response for a timed feed observation.
	if job.prefix == nil || job.unavailable != "" {
		p.expireDelays(c, time.Time{}, "original prefix unavailable for delay comparison")
	}
	return c
}

func (p *executionPaper) schedule(job paperExecutionJob) { p.start(p.capture(job)) }

// start hands a group to the idle worker with every scenario captured so far;
// later ones are queued as frames arrive.
func (p *executionPaper) start(c *paperDelayCapture) {
	p.remaining.Store(int64(1 + len(p.policy.DelaysMS)))
	now := time.Now()
	for _, job := range append([]paperExecutionJob{c.original}, c.captured...) {
		job.queued = now
		p.jobs <- job
	}
	c.captured = nil
	p.pending = nil
	if c.next < len(p.policy.DelaysMS) {
		p.pending = c
	}
}

func (p *executionPaper) finishDelay(c *paperDelayCapture, job paperExecutionJob) {
	c.next++
	if c != p.pending {
		c.captured = append(c.captured, job)
		return
	}
	job.queued = time.Now()
	p.jobs <- job
	if c.next == len(p.policy.DelaysMS) {
		p.pending = nil
	}
}

func (p *executionPaper) delayJob(c *paperDelayCapture) paperExecutionJob {
	job := c.original
	job.delayMS = p.policy.DelaysMS[c.next]
	job.prefix = nil
	job.observed = time.Time{}
	job.executionVersion = domain.StateVersion{}
	return job
}

func (p *executionPaper) expireDelays(c *paperDelayCapture, now time.Time, reason string) {
	for c.next < len(p.policy.DelaysMS) {
		job := p.delayJob(c)
		deadline := job.discovered.Add(time.Duration(job.delayMS)*time.Millisecond + paperCaptureGrace)
		if reason == "" && !now.After(deadline) {
			return
		}
		job.unavailable = reason
		if reason == "" {
			job.unavailable = "no timely later prefix observed"
		}
		job.censored = true
		p.finishDelay(c, job)
	}
}

// expireAll censors overdue scenarios of the running and the waiting group.
func (p *executionPaper) expireAll(now time.Time) {
	for _, c := range []*paperDelayCapture{p.pending, p.waiting} {
		if c != nil {
			p.expireDelays(c, now, "")
		}
	}
}

func (r *runtime) captureExecutionPaper(version domain.StateVersion, received time.Time) {
	p := r.executionPaper
	if p == nil {
		return
	}
	now := time.Now()
	r.scheduleReactiveVerification()
	p.flushSelection(now, false)
	for _, c := range []*paperDelayCapture{p.pending, p.waiting} {
		if c != nil {
			r.captureDelays(c, version, received, now)
		}
	}
}

func (r *runtime) captureDelays(c *paperDelayCapture, version domain.StateVersion, received, now time.Time) {
	p := r.executionPaper
	// Canonical reconciliation does not replace a stream observation.
	if version.Source != domain.SourceFast {
		p.expireDelays(c, now, "")
		return
	}
	for c.next < len(p.policy.DelaysMS) {
		job := p.delayJob(c)
		target := job.discovered.Add(time.Duration(job.delayMS) * time.Millisecond)
		if received.Before(target) {
			break
		}
		job.observed = received
		job.executionVersion = version
		original := c.original.prefix
		canonical := p.policy.PrefixSource == "canonical-trace"
		// Reconciliation changes quote epochs even when feed ancestry is intact.
		// Canonical traces verify header/transaction identity independently.
		sameBlock := paperPrefixExtends(*original, r.flashPrefix) && (canonical || original.Version.Epoch == version.Epoch)
		nextBlock := canonical && paperPrefixNextBlock(*original, r.flashPrefix)
		switch {
		case now.After(target.Add(paperCaptureGrace)):
			job.unavailable = "later prefix processed beyond capture window"
		case !r.flash.active || r.flashPrefix.Version != version:
			job.unavailable = "later prefix is not retained"
		case !sameBlock && !nextBlock:
			job.unavailable = "later prefix crosses block or replaces original branch"
		case len(r.flashPrefix.Transactions) > p.policy.MaxPrefixTransactions:
			job.unavailable = "later prefix exceeds execution paper replay limit"
		default:
			prefix := r.flashPrefix.freeze()
			job.prefix = &prefix
			job.anchor = r.block
			job.crossBlock = nextBlock
		}
		job.censored = job.unavailable != ""
		p.finishDelay(c, job)
	}
	p.expireDelays(c, now, "")
}

func paperPrefixExtends(original, later flashPrefix) bool {
	if later.Payload != original.Payload || !reflect.DeepEqual(later.Base, original.Base) || later.Index <= original.Index || len(later.Transactions) < len(original.Transactions) {
		return false
	}
	for i, tx := range original.Transactions {
		if later.Transactions[i].Hash != tx.Hash {
			return false
		}
	}
	return true
}

// Capturing a possible child is cheap. The serial worker must still bind its
// parent to the baseline's verified canonical block before any simulation.
func paperPrefixNextBlock(original, later flashPrefix) bool {
	return later.Base.Number > original.Base.Number && later.Base.Number-original.Base.Number == 1 &&
		later.Base.Timestamp > original.Base.Timestamp && later.Base.Parent != (domain.Hash{}) &&
		later.Payload != original.Payload
}
