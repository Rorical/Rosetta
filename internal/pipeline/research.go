package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// Research observes immutable pool views on a separate worker. It never reads
// RPC or acquires an EVM lease, and bounded queues cannot stall the pool writer.
// Skipped samples are explicit censored gaps, not proof of continuous profit.
type Research struct {
	frames        chan ResearchFrame
	candidates    chan researchCandidate
	confirmations chan ResearchEvent
	cancel        context.CancelFunc
	done          chan struct{}
	metrics       *telemetry.Registry
	skipped       atomic.Uint64
	mu            sync.Mutex
	err           error
	output        *rotatingFile
	chainID       uint64
	policy        ResearchPolicy
	book          *paperBook
	spot          spotConversions // conversions without a paper book
	auditJobs     chan swapAuditJob
	auditDone     chan struct{}
	auditSkipped  atomic.Uint64
	prefixGaps    prefixGapCounter
	netCounts     economicsCounters
	searchSkipped atomic.Uint64
}

func NewResearch(ctx context.Context, configPath, outputPath string, identities ...domain.ChainIdentity) (*Research, error) {
	if outputPath == "" {
		if configPath != "" {
			return nil, fmt.Errorf("research config requires output")
		}
		return nil, nil
	}
	policy, err := LoadResearchPolicy(configPath)
	if err != nil {
		return nil, err
	}
	var chain domain.ChainIdentity
	if len(identities) > 0 {
		chain = identities[0]
	}
	var book *paperBook
	if policy.Paper != nil {
		book = newPaperBook(*policy.Paper)
		if err := book.restore(chain); err != nil {
			return nil, err
		}
	}
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &Research{frames: make(chan ResearchFrame, 1), candidates: make(chan researchCandidate, 64), confirmations: make(chan ResearchEvent, 64), cancel: cancel, done: make(chan struct{}), output: &rotatingFile{file: file}, chainID: chain.ChainID, policy: policy}
	r.metrics = telemetry.FromContext(ctx)
	r.metrics.Sample(telemetry.Standard, telemetry.ResearchBacklog, func() int64 { return int64(len(r.candidates)) })
	r.book = book
	r.auditJobs, r.auditDone = make(chan swapAuditJob, 1), make(chan struct{})
	go r.runSwapAudit(ctx)
	go func() {
		defer close(r.done)
		var persisted uint64
		persist := func() error {
			if r.book == nil {
				return nil
			}
			var err error
			persisted, err = r.book.persist(chain, persisted)
			return err
		}
		defer func() { r.setError(persist()) }()
		checkpointTicker := time.NewTicker(5 * time.Second)
		defer checkpointTicker.Stop()
		var reported [2][4]uint64
		encoder := json.NewEncoder(r.output)
		state := researchState{policy: policy, emit: func(event ResearchEvent) error {
			if len(identities) > 0 {
				event.ChainID = identities[0].ChainID
			}
			return encoder.Encode(event)
		}}
		if r.book != nil {
			state.economics = r.book.economics
		}
		if err := state.emit(r.session(time.Now())); err != nil {
			r.setError(err)
			return
		}
		ticker := time.NewTicker(min(time.Second, time.Duration(policy.MaxAgeMS)*time.Millisecond))
		defer ticker.Stop()
		for {
			var err error
			select {
			case <-ctx.Done():
				<-r.auditDone
				for draining := true; draining; {
					select {
					case event := <-r.candidates:
						err = state.discover(event.event, time.Now(), event.economics...)
					case event := <-r.confirmations:
						err = state.emit(event)
					default:
						draining = false
					}
					if err != nil {
						r.setError(err)
						return
					}
				}
				if skipped := r.skipped.Swap(0); skipped > 0 {
					for _, row := range state.routes {
						row.continuous = false
					}
					if err = state.emit(ResearchEvent{Type: "gap", AtMS: time.Now().UnixMilli(), Censored: true, Skipped: skipped}); err != nil {
						r.setError(err)
						return
					}
				}
				if skipped := r.auditSkipped.Swap(0); skipped > 0 {
					if err = state.emit(ResearchEvent{Type: "swap_quote_audit_gap", AtMS: time.Now().UnixMilli(), Censored: true, Skipped: skipped}); err != nil {
						r.setError(err)
						return
					}
				}
				if err = r.flushTraceGaps(state.emit, time.Now()); err != nil {
					r.setError(err)
					return
				}

				err = state.finish(time.Now(), "shutdown")
				r.setError(err)
				return
			case <-checkpointTicker.C:
				err = persist()
			case now := <-ticker.C:
				if counts := r.netCounts.snapshot(); counts != reported {
					reported = counts
					err = state.emit(ResearchEvent{Type: "economics_summary", AtMS: now.UnixMilli(), Details: counts.report()})
					if err != nil {
						r.setError(err)
						return
					}
				}
				err = state.expire(now)
				if err == nil {
					err = r.flushTraceGaps(state.emit, now)
				}

			case event := <-r.candidates:
				err = state.discover(event.event, time.Now(), event.economics...)
			case event := <-r.confirmations:
				err = state.emit(event)
				if skipped := r.auditSkipped.Swap(0); err == nil && skipped > 0 {
					err = state.emit(ResearchEvent{Type: "swap_quote_audit_gap", AtMS: time.Now().UnixMilli(), Censored: true, Skipped: skipped})
				}
			case frame := <-r.frames:
				if skipped := r.skipped.Swap(0); skipped > 0 {
					for _, row := range state.routes {
						row.continuous = false
					}
					err = state.emit(ResearchEvent{Type: "gap", AtMS: frame.At.UnixMilli(), Version: frame.Version, Censored: true, Skipped: skipped})
				}
				if err == nil {
					evaluation, cancel := context.WithTimeout(ctx, time.Duration(policy.EvaluationMS)*time.Millisecond)
					err = state.advance(evaluation, frame)
					cancel()
				}
			}
			if err != nil {
				r.setError(err)
				return
			}
		}
	}()
	return r, nil
}
func (r *Research) setError(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = errors.Join(r.err, err)
}
func (r *Research) Advance(frame ResearchFrame) {
	if r == nil {
		return
	}
	if frame.At.IsZero() {
		frame.At = time.Now()
	}
	select {
	case <-r.frames:
		r.dropped()
	default:
	}
	select {
	case r.frames <- frame:
	default:
		r.dropped()
	}
}

type researchCandidate struct {
	event     arb.SearchEvent
	economics []ResearchEconomics
}

func (r *Research) Observe(event arb.SearchEvent) []ResearchEconomics {
	if r == nil {
		return nil
	}
	if len(event.Result.Opportunities) > r.policy.MaxTracked {
		event.Result.Opportunities = event.Result.Opportunities[:r.policy.MaxTracked]
	}
	economics := make([]ResearchEconomics, len(event.Result.Opportunities))
	for i, o := range event.Result.Opportunities {
		economics[i] = r.Economics(event, o)
		r.netCounts.record(event.Stage, economics[i].Status)
	}
	event.Job.Release = nil
	event.Job.Prepare = nil
	event.Job.PrepareExact = nil
	select {
	case r.candidates <- researchCandidate{event, economics}:
	default:
		r.dropped()
	}
	return economics
}
func (r *Research) Confirmation(event ConfirmationEvent) {
	if r == nil {
		return
	}
	status := "rejected"
	if event.Plan != nil {
		status = "simulated_ready"
	}
	details := map[string]any{"status": status, "rejections": event.Rejections}
	if event.Err != nil {
		details["error"] = event.Err.Error()
	}
	if event.Plan != nil {
		details["economics"] = event.Plan.Economics
		details["plan_id"] = event.Plan.ID
	}
	sample := ResearchEvent{Type: "confirmation", AtMS: time.Now().UnixMilli(), Version: event.Identity.Version, Graph: event.Identity.Graph, Details: details}
	if event.Plan != nil {
		cycle := graph.Cycle{Start: event.Plan.Route.Asset, Len: len(event.Plan.Route.Steps)}
		for _, step := range event.Plan.Route.Steps {
			cycle.Pools = append(cycle.Pools, step.Pool)
		}
		sample.ID = routeID(cycle, event.Plan.Route.Amount, event.Identity.Version.Source)
		sample.Cycle = &cycle
		sample.Input = event.Plan.Route.Amount
	}
	select {
	case r.confirmations <- sample:
	default:
		r.dropped()
	}
}
func (r *Research) dropped() {
	r.skipped.Add(1)
	r.metrics.Add(telemetry.Standard, telemetry.ResearchDropped, 1)
}

func (r *Research) Close() error {
	if r == nil {
		return nil
	}
	r.cancel()
	<-r.auditDone
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	file := r.output.current()
	return errors.Join(r.err, file.Sync(), file.Close())
}
