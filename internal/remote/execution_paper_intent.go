package remote

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// paperIntentReference ties a verification record to the shadow intent it
// replays. Join with the reactive output on ID.
type paperIntentReference struct {
	ID          domain.Hash `json:"id"`
	ExpectedNet *big.Int    `json:"expected_net"`
	MinimumNet  *big.Int    `json:"minimum_net"`
	Budget      *big.Int    `json:"budget"`
}

// openSnapshot binds the job's exact quote state: the frozen Flashblocks
// prefix over its direct parent, or the canonical anchor.
func (p *executionPaper) openSnapshot(ctx context.Context, job paperExecutionJob, result *paperExecutionResult, gasLimit uint64) (*snapshot, error) {
	started := time.Now()
	var s *snapshot
	var err error
	if job.prefix != nil {
		// Fetch only the exact direct parent when log-only pool state leads RPC.
		// Never substitute latest or a different prefix for this candidate.
		anchor := job.anchor
		if anchor.Ref.Hash != job.prefix.Base.Parent {
			anchor, err = p.client.ExecutionBlock(ctx, job.prefix.Base.Parent)
			if err != nil {
				return nil, fmt.Errorf("paper prefix parent: %w", err)
			}
		}
		prefix := *job.prefix
		prefix.Version.Anchor = anchor.Ref
		if _, _, err := prefix.rawBlock(); err != nil {
			return nil, err
		}
		if p.policy.PrefixSource == "canonical-trace" {
			s, result.Flash, result.CanonicalTraceBlock, err = p.canonical.open(ctx, p.client, anchor, p.chain, p.deployments, prefix)
		} else {
			s, result.Flash, err = p.source.open(ctx, p.client, anchor, p.chain, p.deployments, prefix)
		}
	} else {
		s = &snapshot{client: p.client, block: job.anchor, chain: p.chain, deployments: p.deployments}
		err = s.checkCanonical(ctx)
	}
	if err != nil {
		return nil, err
	}
	s.risks = p.risks
	if result.CanonicalTraceBlock != nil {
		s.riskBlock = *result.CanonicalTraceBlock
	}
	result.SnapshotNS = time.Since(started).Nanoseconds()
	if result.Flash != nil && gasLimit > result.Flash.BlockGasLeft {
		s.Close()
		return nil, fmt.Errorf("paper gas envelope exceeds prefix remaining gas")
	}
	return s, nil
}

// execute runs one prepared invocation on the snapshot and values the outcome
// against the job's candidate. A revert counts as zero profit, never a loss of
// principal; its network fee stays in TotalFee.
func (p *executionPaper) execute(ctx context.Context, s *snapshot, job paperExecutionJob, request execution.ConfirmationRequest, wrapped domain.Address, result *paperExecutionResult) error {
	started := time.Now()
	if err := s.executePaper(ctx, request, result); err != nil {
		return err
	}
	result.ExecutionNS = time.Since(started).Nanoseconds()
	if result.CanonicalTraceBlock != nil {
		result.Competition = p.canonical.competition(ctx, p.client, *job.prefix, job.opportunity)
	}
	profit, loan := result.Profit, result.LoanFee
	if result.Status == "reverted" {
		profit, loan = new(big.Int), new(big.Int)
	}
	economics := p.research.ExecutionEconomics(job.event.Job.View, job.opportunity, wrapped, profit, loan, result.TotalFee)
	result.Economics = &economics
	return nil
}

// evaluateIntent replays a sealed shadow intent as live would send it: the
// configured sender calls the deployed executor with the intent's plan and fee
// envelope. Only the nonce comes from the snapshot, because shadow never
// reserves one. Nothing is deployed or funded; balances and approvals are real.
func (p *executionPaper) evaluateIntent(ctx context.Context, job paperExecutionJob, result *paperExecutionResult) error {
	plan := job.intent.Route
	result.Route = &plan
	params := job.intent.Call.CallParameters
	s, err := p.openSnapshot(ctx, job, result, params.GasLimit)
	if err != nil {
		return err
	}
	defer s.Close()
	if params.Nonce, err = s.Nonce(ctx, params.Sender); err != nil {
		return err
	}
	wrapped, _ := domain.ParseAddress(p.policy.WrappedNative)
	if err := p.execute(ctx, s, job, execution.ConfirmationRequest{Plan: plan, Parameters: params}, wrapped, result); err != nil {
		return err
	}
	if result.CanonicalTraceBlock != nil {
		return verifyPaperCanonical(ctx, p.client, *result.CanonicalTraceBlock)
	}
	return nil
}

// scheduleReactiveVerification runs on the main loop, which owns paper
// scheduling. A new shadow intent starts capturing its later prefixes at once,
// even while the worker is busy; only the newest waiting group is kept. It
// outranks sampling: an open selection window is dropped, while a running
// group is never interrupted.
func (r *runtime) scheduleReactiveVerification() {
	p := r.executionPaper
	if p == nil || r.reactive == nil || p.error() != nil {
		return
	}
	if job := r.reactive.takeVerification(); job != nil {
		if p.waiting != nil {
			r.reactive.skip("verification_superseded")
		}
		p.waiting = p.capture(*job)
	}
	if p.waiting == nil || p.window == nil && !p.busy.CompareAndSwap(false, true) {
		return
	}
	if p.window != nil {
		p.sampler.skip(p.window.job.opportunity, p.window.job.event.Job.Version, p.window.job.discovered, "reactive_verification_preempted")
		p.window = nil
	}
	c := p.waiting
	p.waiting = nil
	p.start(c)
}
