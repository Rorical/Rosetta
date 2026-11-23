package remote

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func (d *poolDiscovery) build(ctx context.Context, r *runtime, job discoveryTask) (answer discoveryAnswer) {
	ctx = job.ctx
	if ctx == nil {
		answer.err = fmt.Errorf("discovery task has no context")
		return
	}
	if job.generation != d.scanGeneration {
		if d.index != nil {
			d.index.Reset()
		} else if logs, ok := d.logs.(*discovery.WindowLogs); ok {
			logs.Reset()
		}
		d.scanGeneration = job.generation
		d.probe = discovery.ActiveProbe{}
		// Verdicts were made on the abandoned branch; like the index they restart.
		d.verdicts = nil
		d.candidates = discovery.CandidateQueue{}
		d.markets.Reset()
	}
	answer.anchor = job.reader.block.Ref
	answer.graph = job.graph
	answer.generation = job.generation
	if err := d.awaitStateAnchor(ctx, answer.anchor, 30*time.Second, 500*time.Millisecond); err != nil {
		answer.err = err
		return
	}
	candidates, err := d.candidateCreations(ctx, job)
	if err != nil {
		answer.err = err
		return
	}
	var admitted []discovery.Creation
	var specs []bootstrap.Spec
	shallow := job.view.Fork().Pools()
	for _, row := range candidates {
		if err := ctx.Err(); err != nil {
			answer.err = err
			return
		}
		key := row.Spec.PoolKey()
		anchor := job.reader.block.Ref.Number
		spec, active, cached, err := d.identity(ctx, job.reader, row)
		if err != nil {
			if discovery.ProbeRejected(err) {
				d.reject(key, anchor)
				continue
			}
			answer.err = err
			return
		}
		if !active || spec.Fee > d.config.MaxFee {
			d.reject(key, anchor)
			continue
		}
		denied, err := d.deniedToken(ctx, job.reader, spec.Token0)
		if err == nil && !denied {
			denied, err = d.deniedToken(ctx, job.reader, spec.Token1)
		}
		if err != nil {
			answer.err = err
			return
		}
		if denied {
			d.reject(key, anchor)
			continue
		}
		state, err := bootstrap.LoadShallow(ctx, job.reader, spec, fmt.Sprintf("0x%x", anchor))
		if err != nil {
			answer.err = err
			return
		}
		if cached && !poolActive(state) || state.Fee > d.config.MaxFee || state.FeeOneForZero != nil && *state.FeeOneForZero > d.config.MaxFee {
			d.reject(key, anchor)
			continue
		}
		spec.Fee, spec.TickSpacing = state.Fee, state.TickSpacing
		row.Verified = true
		row.Spec.Fee = state.Fee
		row.Spec.TickSpacing = state.TickSpacing
		specs = append(specs, spec)
		shallow = append(shallow, state)
		admitted = append(admitted, row)
	}
	var report []discovery.Assessment
	selected := map[domain.PoolKey]discovery.Assessment{}
	if d.config.Selection != nil {
		selection, err := d.markets.Select(ctx, shallow, job.activity, *d.config.Selection, d.config.MaxPools, d.config.Selection.Depth(r.policy.Depth), discovery.PinnedMarketKeys(job.view.PoolKeys(), d.config.Seeds, shallow))
		if err != nil {
			answer.err = err
			return
		}
		if selection.Truncated {
			slog.Info("market selection capped dense candidates", "candidates", selection.CappedCandidates)
		}
		report = selection.Assessments
		for _, row := range selection.Assessments {
			if row.Reason == "selected" || row.Reason == "pinned" {
				selected[row.Pool] = row
			}
		}
	} else {
		for _, p := range shallow {
			selected[p.Key] = discovery.Assessment{Pool: p.Key, Reason: "selected"}
		}
	}
	added := poolstate.New()
	if err := added.BeginBlock(job.reader.block.Ref); err != nil {
		answer.err = err
		return
	}
	added.Commit()
	var addedKeys []domain.PoolKey

	for _, spec := range specs {
		key := domain.PoolKey{Address: spec.Address, Kind: spec.Kind, PoolID: spec.PoolID, Hooks: spec.Hooks}
		row, ok := selected[key]
		if !ok {
			continue
		}
		loaded := poolstate.New()
		if err := bootstrap.Seed(ctx, job.reader, loaded, []bootstrap.Spec{spec}, job.reader.block.Ref, r.policy.WordRadius); err != nil {
			answer.err = err
			return
		}
		state, _ := loaded.Pool(key)
		deep, _ := loaded.View().Deep(key)
		if d.config.Selection != nil && row.TargetIn != nil {
			if err := discovery.VerifyDepth(ctx, state, deep, row, d.config.Selection.MaxSlippageBPS); err != nil {
				continue
			}
		}
		if err := added.Upsert(state); err != nil {
			answer.err = err
			return
		}
		if key.Kind.UsesTicks() {
			if err := added.SetDeep(key, deep); err != nil {
				answer.err = err
				return
			}
		}
		addedKeys = append(addedKeys, key)

	}
	// A failed full quote can remove a companion edge. Recheck whole-cycle
	// coverage before installing any of the surviving additions.
	if d.config.Selection != nil && len(addedKeys) > 0 {
		final, err := d.markets.Select(ctx, append(job.view.Fork().Pools(), added.Pools()...), job.activity, *d.config.Selection, d.config.MaxPools, d.config.Selection.Depth(r.policy.Depth), discovery.PinnedMarketKeys(job.view.PoolKeys(), d.config.Seeds, append(job.view.Fork().Pools(), added.Pools()...)))
		if err != nil {
			answer.err = err
			return
		}
		keep := map[domain.PoolKey]bool{}
		for _, key := range final.Keys {
			keep[key] = true
		}
		pruned := poolstate.New()
		if err := pruned.BeginBlock(job.reader.block.Ref); err != nil {
			answer.err = err
			return
		}
		pruned.Commit()
		addedKeys = nil
		for _, p := range added.Pools() {
			if !keep[p.Key] {
				continue
			}
			if err := pruned.Upsert(p); err != nil {
				answer.err = err
				return
			}
			if deep, ok := added.View().Deep(p.Key); ok {
				if err := pruned.SetDeep(p.Key, deep); err != nil {
					answer.err = err
					return
				}
			}
			addedKeys = append(addedKeys, p.Key)
		}
		added = pruned
		report = final.Assessments
	}
	// Recheck after all state reads before publishing index or graph metadata.
	current, err := job.reader.client.ExecutionBlock(ctx, fmt.Sprintf("0x%x", job.reader.block.Ref.Number))
	if err != nil {
		answer.err = err
		return
	}
	if current.Ref.Hash != job.reader.block.Ref.Hash {
		answer.err = fmt.Errorf("discovery anchor ceased canonical")
		return
	}
	// Candidates that verified but were not selected wait one refresh interval,
	// not a full activity window: a multi-hop cycle through a non-funding token
	// only forms once all its pools are admitted together, and they verify in
	// different rounds as the backfill reaches them.
	adding := make(map[domain.PoolKey]bool, len(addedKeys))
	for _, key := range addedKeys {
		adding[key] = true
	}
	for _, row := range admitted {
		if key := row.Spec.PoolKey(); !adding[key] {
			d.deferUnselected(key, job.reader.block.Ref.Number)
		}
	}
	if d.index != nil {
		d.saveVerdicts(job.reader.block.Ref.Number)
		d.index.Remember(admitted)
		d.index.SetMarketReport(job.activityRecords, discovery.DiscoveryReport(d.probe.Report, report))
		if err := d.index.Save(ctx, job.reader.block.Ref); err != nil {
			answer.err = err
			return
		}
	}
	if len(addedKeys) == 0 {
		return
	}
	added.Commit()
	added.ClearDirty()
	answer.added = added
	states := append(job.view.Fork().Pools(), added.Pools()...)
	// Topology is independent of mutable prices. Tail replay on adoption updates
	// state while retaining these pool identities and path indices.
	answer.topology, answer.err = pipeline.ExtendTopology(ctx, states, job.topology, addedKeys, r.topologyOptions())
	return
}
