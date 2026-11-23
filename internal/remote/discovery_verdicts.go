package remote

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// discoveryUrgentGap is the least time between a round and an urgent one.
// A factory or V4 Initialize log marks the next round urgent; without a gap a
// steady stream of creations ran rounds back to back, each a thousand reads.
const discoveryUrgentGap = 5 * time.Minute

// maxVerdicts matches the index file's record limit.
const maxVerdicts = 100000

// rejectBlocks is how long a rejected, inactive or unselected candidate is left
// out: one activity window, the horizon its swap count is measured over.
func (d *poolDiscovery) rejectBlocks() uint64 {
	if d.config.Selection != nil && d.config.Selection.ActivityWindow > 0 {
		return d.config.Selection.ActivityWindow
	}
	return max(d.config.RefreshBlocks, 1)
}

// cooling reports whether key was rejected recently enough to skip.
func (d *poolDiscovery) cooling(key domain.PoolKey, anchor uint64) bool {
	return d.verdicts[key].Until > anchor
}

// identity returns the pool's verified metadata, reading it only once. active
// is the verification's liquidity check; a cached identity reports true and
// leaves activity to the caller's fresh state.
func (d *poolDiscovery) identity(ctx context.Context, reader *snapshot, row discovery.Creation) (spec bootstrap.Spec, active, cached bool, err error) {
	if row.Source == 0 {
		// Seeds are checked against the configuration, without reads; a cached
		// verdict must not outlive the seed's removal.
		spec, active, err = d.verify(ctx, reader, row)
		return spec, active, false, err
	}
	key := row.Spec.PoolKey()
	if verdict, ok := d.verdicts[key]; ok && verdict.Spec != nil {
		return *verdict.Spec, true, true, nil
	}
	spec, active, err = d.verify(ctx, reader, row)
	if err == nil {
		d.remember(key, discovery.Verdict{Key: key, Spec: &spec})
	}
	return spec, active, false, err
}

// reject leaves key out until rejectBlocks after anchor, keeping any verified identity.
func (d *poolDiscovery) reject(key domain.PoolKey, anchor uint64) {
	verdict := d.verdicts[key]
	verdict.Key, verdict.Until = key, anchor+d.rejectBlocks()
	d.remember(key, verdict)
}

// deferUnselected benches a pool that verified but that no funded cycle picked
// this round for one refresh interval, not a full activity window. A multi-hop
// cycle through a non-funding token only forms once every pool on it is
// admitted together; its pools verify in different rounds as the V4 Initialize
// backfill reaches them, so a long bench makes them keep missing each other. The verified identity is kept, so the retry needs no reads.
func (d *poolDiscovery) deferUnselected(key domain.PoolKey, anchor uint64) {
	verdict := d.verdicts[key]
	verdict.Key, verdict.Until = key, anchor+max(d.config.RefreshBlocks, 1)
	d.remember(key, verdict)
}

func (d *poolDiscovery) remember(key domain.PoolKey, verdict discovery.Verdict) {
	if d.verdicts == nil {
		d.verdicts = make(map[domain.PoolKey]discovery.Verdict)
	}
	if _, ok := d.verdicts[key]; !ok && len(d.verdicts) >= maxVerdicts {
		return
	}
	d.verdicts[key] = verdict
}

// poolActive repeats the verification's liquidity check on loaded state.
func poolActive(p pool.State) bool {
	switch p.Key.Kind {
	case domain.PoolKindV2, domain.PoolKindVelodromeV2:
		return p.Reserve0 != nil && p.Reserve1 != nil && p.Reserve0.Sign() > 0 && p.Reserve1.Sign() > 0
	case domain.PoolKindV4:
		return true // verification checks the derived ID, not liquidity
	default:
		return p.Liquidity != nil && p.Liquidity.Sign() > 0
	}
}

// saveVerdicts hands live outcomes and probe cooldowns to the index; expired
// rejections without an identity are dropped.
func (d *poolDiscovery) saveVerdicts(anchor uint64) {
	verdicts := make([]discovery.Verdict, 0, len(d.verdicts))
	for key, verdict := range d.verdicts {
		if verdict.Spec == nil && verdict.Until <= anchor {
			delete(d.verdicts, key)
			continue
		}
		verdicts = append(verdicts, verdict)
	}
	probes := make([]discovery.Cooldown, 0, len(d.probe.After))
	for key, until := range d.probe.After {
		if until > anchor {
			probes = append(probes, discovery.Cooldown{Key: key, Until: until})
		}
	}
	d.index.SetVerdicts(discovery.VerdictIdentity(d.chainID, d.config), verdicts, probes, d.probe.ScanStates())
}

// restoreVerdicts loads the outcomes saved with a still-canonical index.
func (d *poolDiscovery) restoreVerdicts() {
	verdicts, probes := d.index.Verdicts(discovery.VerdictIdentity(d.chainID, d.config))
	for _, verdict := range verdicts {
		d.remember(verdict.Key, verdict)
	}
	if len(probes) > 0 {
		d.probe.After = make(map[domain.PoolKey]uint64, len(probes))
		for _, cooldown := range probes {
			d.probe.After[cooldown.Key] = cooldown.Until
		}
	}
	d.probe.RestoreScans(d.index.V4Scans(discovery.VerdictIdentity(d.chainID, d.config)))
}

const maxDeniedTokens = 8192

// deniedToken reports whether a token's code matches a denied template,
// reading each token's code once.
func (d *poolDiscovery) deniedToken(ctx context.Context, reader *snapshot, token domain.Address) (bool, error) {
	if len(d.config.DenyTokenCode) == 0 {
		return false, nil
	}
	if denied, ok := d.deniedTokens[token]; ok {
		return denied, nil
	}
	code, err := reader.Code(ctx, token)
	if err != nil {
		return false, fmt.Errorf("token code %s: %w", token.Hex(), err)
	}
	denied := d.config.DeniedTokenCode(code)
	if d.deniedTokens == nil || len(d.deniedTokens) >= maxDeniedTokens {
		d.deniedTokens = make(map[domain.Address]bool)
	}
	d.deniedTokens[token] = denied
	if denied {
		slog.Warn("discovery rejects token by code template", "token", token.Hex())
	}
	return denied, nil
}
