package remote

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

type flashConfirmationTask struct {
	source      *flashSnapshotSource
	client      *rpc.Client
	anchor      rpc.ExecutionBlock
	chain       domain.ChainIdentity
	deployments domain.Deployments
	prefix      flashPrefix
	batch       planning.Batch
	policy      planning.Policy
}

func (t *flashConfirmationTask) Run(ctx context.Context) (*execution.ReadyPlan, []string, error) {
	snapshot, proof, err := t.source.open(ctx, t.client, t.anchor, t.chain, t.deployments, t.prefix)
	if err != nil {
		return nil, nil, err
	}
	defer snapshot.Close()
	t.batch.Snapshot, t.batch.FlashProof = snapshot, proof
	return planning.Build(ctx, t.batch, t.policy, time.Now)
}
func (t *flashConfirmationTask) Close() {}

func (r *runtime) confirmFlashCandidates(event arb.SearchEvent, opportunities []arb.Opportunity) error {
	if r.flashConfirmation == nil || !r.flash.active || r.flashPrefix.Version != event.Job.Version || len(opportunities) == 0 {
		return nil
	}
	// Multi-block pool quotes may lead RPC. Exact execution still needs the
	// current payload's direct verified parent, never an older replay anchor.
	if r.flashPrefix.Base.Parent != r.block.Ref.Hash || r.flashPrefix.Base.Number != r.block.Ref.Number+1 {
		return nil
	}
	task := &flashConfirmationTask{
		source: r.flashSnapshots,
		client: r.client, anchor: r.block,
		chain:       domain.ChainIdentity{Family: r.cfg.Network.Family, ChainID: r.cfg.ChainID()},
		deployments: r.cfg.Network.DEX, prefix: r.flashPrefix.freeze(), policy: r.execution,
		batch: planning.Batch{
			Version: event.Job.Version, Graph: event.Job.GraphVersion,
			View: event.Job.View, Opportunities: opportunities,
		},
	}
	job := pipeline.ConfirmationJob{Identity: pipeline.ConfirmationIdentity{Version: event.Job.Version, Graph: event.Job.GraphVersion, ReceivedAt: event.Job.ReceivedAt}, Task: task}
	if event.Partial {
		accepted, err := r.flashConfirmation.Offer(job, r.execution.MaxCandidates)
		if !accepted {
			task.Close()
		}
		return err
	}
	return r.flashConfirmation.Submit(job)
}
func (r *runtime) consumeFlashConfirmation(ctx context.Context, event pipeline.ConfirmationEvent) error {
	if r.flashConfirmation == nil || !r.flashConfirmation.Current(event) || !r.Current(event.Identity.Version) {
		return nil
	}
	r.research.Confirmation(event)
	if event.Err != nil {
		slog.Warn("Flashblocks candidate confirmation failed", "error", event.Err)
		return nil
	}
	if event.Plan == nil {
		return nil
	}
	current := execution.FlashblocksDispatchContext{Version: r.tentative, PayloadID: r.flash.payload, Index: r.flash.index, Source: r.flashPrefix.Source}
	if err := execution.ValidateFlashblocksForDispatch(ctx, event.Plan, current, r.cfg.ChainID(), r.search.GraphVersion, r.execution.PolicyHash, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("Flashblocks plan validation: %w", err)
	}
	if r.discovery != nil {
		r.discovery.activity.Profitable(pipeline.ProfitablePlanPools(event.Plan))
	}
	return r.deliver(ctx, telemetry.Fast, event.Plan, event.Identity.ReceivedAt)
}
