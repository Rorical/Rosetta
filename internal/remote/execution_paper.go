package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/poolrisk"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

type paperExecutionJob struct {
	crossBlock       bool
	discoveryBlock   *domain.BlockRef
	selection        *paperSelection
	event            arb.SearchEvent
	opportunity      arb.Opportunity
	lenders          []pool.State
	discovered       time.Time
	anchor           rpc.ExecutionBlock
	prefix           *flashPrefix
	unavailable      string
	censored         bool
	group            domain.Hash
	delayMS          int64
	observed         time.Time
	queued           time.Time
	executionVersion domain.StateVersion
	quoteTimestamp   uint64
	quoteBaseFee     *big.Int
	// intent is a sealed reactive intent replayed exactly instead of a paper route.
	intent *execution.ReactiveIntent
}

type executionPaper struct {
	admissionFee atomic.Pointer[paperAdmissionFee]
	risks        *tokenrisk.Registry
	pools        *poolrisk.Registry
	survival     *routeSurvival // nil in tests
	poolOutcomes poolrisk.Tracker
	priority     paperPriorityGate
	window       *paperSelectionWindow
	sampler      paperSampler
	canonical    paperCanonicalSource
	setups       map[domain.Hash]paperSetupCache
	backoff      atomic.Int64
	policy       executionPaperPolicy
	artifact     paperArtifact
	chain        domain.ChainIdentity
	deployments  domain.Deployments
	client       *rpc.Client
	research     *pipeline.Research
	source       flashSnapshotSource
	jobs         chan paperExecutionJob
	done         chan struct{}
	cancel       context.CancelFunc
	busy         atomic.Bool
	skipped      atomic.Uint64
	remaining    atomic.Int64
	pending      *paperDelayCapture
	waiting      *paperDelayCapture // shadow intent group captured while the worker is busy
	mu           sync.Mutex
	err          error
	// offer is called only by the pool owner. A busy worker is never replaced
	// by a newer frame. One candidate and its bounded arrival scenarios are retained.
	next time.Time
}

func newExecutionPaper(ctx context.Context, path string, client *rpc.Client, chain domain.ChainIdentity, deployments domain.Deployments, research *pipeline.Research, risks *tokenrisk.Registry, pools *poolrisk.Registry, survival *routeSurvival) (*executionPaper, error) {
	policy, artifact, err := loadExecutionPaper(path)
	if err != nil {
		return nil, err
	}
	if chain.Family != domain.FamilyOPStack {
		return nil, fmt.Errorf("execution paper currently requires OP Stack RPC")
	}
	if endpoint := os.Getenv("OP_EXECUTION_RPC_URL"); endpoint != "" {
		client = rpc.NewClient(endpoint, rpc.Options{Timeout: 10 * time.Second})
		var text string
		if err := client.Call(ctx, "eth_chainId", []any{}, &text); err != nil {
			return nil, err
		}
		id, err := quantity(text)
		if err != nil || id != chain.ChainID {
			return nil, fmt.Errorf("execution paper RPC chain mismatch")
		}
	}
	var priority paperPriorityGate
	if err := priority.configure(policy.Priority); err != nil {
		return nil, err
	}
	file, err := openPaperJournal(policy.Output, chain, artifact.hash)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &executionPaper{priority: priority, policy: policy, artifact: artifact, chain: chain, deployments: deployments, client: client, research: research, jobs: make(chan paperExecutionJob, 1+len(policy.DelaysMS)), done: make(chan struct{}), cancel: cancel, risks: risks, pools: pools, survival: survival}
	go p.run(ctx, file)
	return p, nil
}
func (p *executionPaper) Close() error {
	if p == nil {
		return nil
	}
	p.flushSelection(time.Now(), true)
	if p.pending != nil {
		p.expireDelays(p.pending, time.Time{}, "shutdown before later prefix capture")
	}
	close(p.jobs)
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
func (p *executionPaper) error() error { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func (p *executionPaper) run(ctx context.Context, file *os.File) {
	defer close(p.done)
	defer func() { p.mu.Lock(); p.err = errors.Join(p.err, file.Sync(), file.Close()); p.mu.Unlock() }()
	encoder := json.NewEncoder(file)
	defer func() {
		if summary := p.sampler.drain(); summary != nil {
			p.mu.Lock()
			p.err = errors.Join(p.err, encoder.Encode(summary))
			p.mu.Unlock()
		}
	}()
	var failedGroup domain.Hash
	var baseline paperDelayBaseline
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		var job paperExecutionJob
		select {
		case <-ticker.C:
			if summary := p.sampler.drain(); summary != nil {
				err := encoder.Encode(summary)
				if err == nil {
					err = file.Sync()
				}
				if err != nil {
					p.mu.Lock()
					p.err = err
					p.mu.Unlock()
					return
				}
			}
			continue
		case next, ok := <-p.jobs:
			if !ok {
				return
			}
			job = next
		}
		if job.delayMS > 0 && job.group != (domain.Hash{}) && job.group == failedGroup && !job.censored {
			job.censored, job.unavailable = true, "earlier scenario could not be verified; group stopped"
		}
		baseline.bind(&job)
		task, cancel := context.WithTimeout(ctx, time.Duration(p.policy.TimeoutMS)*time.Millisecond)
		result := p.evaluate(task, job)
		cancel()
		if p.survival != nil && (result.Status == "success" || result.Status == "reverted") {
			final := len(p.policy.DelaysMS) > 0 && job.delayMS == p.policy.DelaysMS[len(p.policy.DelaysMS)-1]
			p.survival.observe(result.Group, len(job.opportunity.Cycle.Pools), final, job.delayMS, result.Status == "success")
		}
		if job.delayMS == 0 {
			baseline.observe(result)
			if err := p.observePools(job, result); err != nil {
				p.mu.Lock()
				p.err = err
				p.mu.Unlock()
				p.busy.Store(false)
				return
			}
		}
		if !job.censored && (result.StateBasis == "unverified" || result.Status == "rejected_fees" || result.Status == "rejected_fee_unknown" || result.Status == "rejected_token") {
			failedGroup = job.group
		}
		err := encoder.Encode(result)
		if summary := p.sampler.drain(); err == nil && summary != nil {
			err = encoder.Encode(summary)
		}
		if skipped := p.skipped.Swap(0); err == nil && skipped > 0 {
			err = encoder.Encode(map[string]any{"type": "sampling_gap", "basis": "executor_simulation", "skipped": skipped, "at_ns": time.Now().UnixNano()})
		}
		if err == nil {
			err = file.Sync()
		}
		if err != nil {
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
			p.busy.Store(false)
			return
		}
		if p.remaining.Add(-1) <= 0 {
			p.busy.Store(false)
		}
	}
}

func (p *executionPaper) evaluate(ctx context.Context, job paperExecutionJob) (result paperExecutionResult) {
	var calls atomic.Uint64
	ctx = rpc.WithRequestObserver(ctx, func(observation rpc.RequestObservation) { calls.Add(observation.Calls) })
	defer func() { used := calls.Load(); result.RPCCalls = &used }()
	result = paperExecutionResult{Type: "execution", Schema: 1, Basis: "executor_simulation", Chain: p.chain, Artifact: p.artifact.hash, Version: job.event.Job.Version, DiscoveredAtNS: job.discovered.UnixNano(), FeedReceivedAtNS: job.event.Job.ReceivedAt.UnixNano(), StartedAtNS: time.Now().UnixNano(), Status: "unavailable", QuotedGross: job.opportunity.Net}
	identity, _ := json.Marshal([]any{p.chain, p.artifact.hash, result.Version, job.opportunity.Cycle, job.opportunity.Input, result.DiscoveredAtNS})
	result.Group = job.group
	if result.Group == (domain.Hash{}) {
		result.Group = digest(identity)
	}
	result.DelayMS = job.delayMS
	result.PlannedDelaysMS = p.policy.DelaysMS
	if !job.queued.IsZero() {
		result.QueuedAtNS = job.queued.UnixNano()
	}
	result.ExecutionVersion = job.executionVersion
	if !job.observed.IsZero() {
		result.ObservedAtNS = job.observed.UnixNano()
	}
	if job.delayMS > 0 && !job.observed.IsZero() {
		result.ActualDelayNS = job.observed.Sub(job.discovered).Nanoseconds()
	}
	identity, _ = json.Marshal([]any{result.Group, job.delayMS})
	result.ID = digest(identity)
	result.Candidate = &job.opportunity
	result.DiscoveryCanonicalBlock = job.discoveryBlock
	result.Selection = job.selection
	result.Economics = &pipeline.ResearchEconomics{Status: pipeline.NetUnknown, Basis: "executor_simulation"}
	if job.intent != nil {
		result.Type = "reactive_verification"
		result.Intent = &paperIntentReference{ID: job.intent.ID, ExpectedNet: job.intent.ExpectedNet, MinimumNet: job.intent.MinimumNet, Budget: job.intent.Budget}
	}
	if job.censored {
		result.Status, result.StateBasis, result.Error = "censored", "unverified", job.unavailable
		result.FinishedAtNS = time.Now().UnixNano()
		return result
	}
	err := p.evaluateAt(ctx, job, &result)
	if err != nil {
		result.Error = err.Error()
		if result.Status != "unsupported" && result.Status != "rejected_funding" && job.unavailable == "" {
			p.backoff.Store(time.Now().Add(time.Minute).UnixMilli())
		}
		result.StateBasis = "unverified"
	} else {
		result.StateBasis = "discovery_snapshot"
		if job.delayMS > 0 {
			result.StateBasis = "later_observed_prefix"
		}
	}
	result.FinishedAtNS = time.Now().UnixNano()
	return result
}
func (p *executionPaper) evaluateAt(ctx context.Context, job paperExecutionJob, result *paperExecutionResult) error {
	if job.unavailable != "" {
		return errors.New(job.unavailable)
	}
	if job.intent != nil {
		return p.evaluateIntent(ctx, job, result)
	}
	wrapped, _ := domain.ParseAddress(p.policy.WrappedNative)
	timestamp := job.anchor.Ref.Timestamp
	if job.prefix != nil {
		timestamp = job.prefix.Base.Timestamp
	}
	if job.quoteTimestamp != 0 {
		timestamp = job.quoteTimestamp
	}
	tip, ok := new(big.Int).SetString(p.policy.TipWei, 10)
	if !ok {
		result.Status = "unsupported"
		return fmt.Errorf("invalid paper priority fee")
	}
	if tip.Sign() > 0 && planning.HasTransactionFees(job.opportunity) {
		baseFee := job.quoteBaseFee
		if baseFee == nil {
			baseFee = job.anchor.BaseFee
		}
		quoted, fees, err := planning.QuoteTransaction(ctx, job.event.Job.View, job.opportunity, baseFee, tip, 0)
		if err != nil {
			result.Status = "unsupported"
			return err
		}
		result.TransactionQuote, result.PoolFees = &quoted, fees
		// Keep the original candidate in the result for discovery comparisons.
		// Admission, calldata guards and model calibration use the signed tip.
		job.opportunity = quoted
		result.QuotedGross = quoted.Net
	}
	plan, err := paperRoute(ctx, job.event.Job.View, job.opportunity, job.lenders, wrapped, p.policy.SlippageBPS, timestamp)
	if err != nil {
		result.Status = "unsupported"
		if errors.Is(err, planning.ErrFundingUnprofitable) {
			result.Status = "rejected_funding"
		}
		return err
	}
	if err := execution.CheckRouteTokens(plan, p.risks.Check); err != nil {
		result.Status, result.Error = "rejected_token", err.Error()
		return nil
	}
	for _, step := range plan.Steps {
		if err := p.pools.Check(step.Pool); err != nil {
			result.Status, result.Error = "rejected_pool", err.Error()
			return nil
		}
	}
	result.Route = &plan
	s, err := p.openSnapshot(ctx, job, result, p.policy.GasLimit)
	if err != nil {
		return err
	}
	defer s.Close()
	// A deterministic virtual EOA belongs exclusively to this simulation.
	senderHash := digest([]byte(fmt.Sprintf("rosetta.execution-paper.operator.%d", p.chain.ChainID)))
	var sender domain.Address
	copy(sender[:], senderHash[12:])
	feeTask := s.startPaperFeePrefetch(ctx, p.policy.GasLimit)
	setupStarted := time.Now()
	executor, err := p.setup(ctx, s, sender)
	feeResult := <-feeTask
	result.FeePrefetchNS = feeResult.duration.Nanoseconds()
	result.SetupNS = time.Since(setupStarted).Nanoseconds()
	err = errors.Join(err, feeResult.err)
	if err != nil {
		return err
	}
	if err := s.fundPaperWallet(ctx, sender, executor, wrapped, plan); err != nil {
		return err
	}
	code, err := s.CodeHash(ctx, executor)
	if err != nil {
		return err
	}
	nonce, err := s.Nonce(ctx, sender)
	if err != nil {
		return err
	}
	baseFee := job.quoteBaseFee
	if baseFee == nil {
		baseFee = s.block.BaseFee
	}
	fee := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)
	params := execution.CallParameters{Sender: sender, Executor: executor, CodeHash: code, Nonce: nonce, GasLimit: p.policy.GasLimit, FeeCap: fee, TipCap: tip}
	if accepted, err := p.preflightPaperFees(ctx, s, job, plan, params, result); err != nil || !accepted {
		return err
	}
	if err := p.execute(ctx, s, job, execution.ConfirmationRequest{Plan: plan, Parameters: params}, wrapped, result); err != nil {
		return err
	}
	result.Bidding = p.bidBudget(ctx, s, job, result, wrapped)
	if result.CanonicalTraceBlock != nil {
		if err := verifyPaperCanonical(ctx, p.client, *result.CanonicalTraceBlock); err != nil {
			return err
		}
	}
	attachPaperFeeDiagnostic(ctx, s, job, result, sender)
	return nil
}
