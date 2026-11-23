package discovery

import (
	"context"
	"fmt"
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	poolmath "github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// SelectionPolicy compares depth at explicit target trade sizes. Amounts are
// raw token units, so callers choose comparable capital amounts, not decimals.
// Virtual-reserve impact is a shortlist estimate; admission still needs real quotes.
type SelectionPolicy struct {
	Targets        []SelectionTarget `json:"targets"`
	MaxTokens      int               `json:"max_tokens"`
	MaxSlippageBPS uint64            `json:"max_slippage_bps"`
	MinSwaps       uint64            `json:"min_swaps"`
	ActivityWindow uint64            `json:"activity_window"`
	ProbeLimit     int               `json:"probe_limit"`
	// MaxDepth admits pools whose only funded cycles are longer than the
	// search table's depth; the background explorer quotes those cycles.
	// Zero uses the search depth.
	MaxDepth int `json:"max_depth,omitempty"`
}

// Depth is the cycle length that admits a pool, given the search depth.
func (p SelectionPolicy) Depth(search int) int {
	if p.MaxDepth > 0 {
		return p.MaxDepth
	}
	return search
}

type SelectionTarget struct {
	Token  string `json:"token"`
	Amount string `json:"amount"`
}
type Assessment struct {
	Pool      domain.PoolKey `json:"pool"`
	Reason    string         `json:"reason"`
	ImpactBPS uint64         `json:"estimated_impact_bps"`
	Activity  Activity       `json:"activity"`
	TargetIn  *big.Int       `json:"target_input,omitempty"`
	TokenIn   domain.Address `json:"token_in"`
}
type Selection struct {
	Keys        []domain.PoolKey
	Assessments []Assessment
	// Truncated reports that some candidate filled its cycle budget or the
	// global memory bound was reached; CappedCandidates counts the former.
	Truncated        bool
	CappedCandidates int
}

func (p SelectionPolicy) Validate() error {
	if len(p.Targets) == 0 || len(p.Targets) > 32 || p.MaxTokens < 2 || p.MaxTokens > 256 || p.MaxSlippageBPS == 0 || p.MaxSlippageBPS >= 10000 || p.ActivityWindow == 0 || p.ProbeLimit < 1 || p.ProbeLimit > 4096 || p.MaxDepth != 0 && (p.MaxDepth < 2 || p.MaxDepth > 8) {
		return fmt.Errorf("invalid discovery selection limits")
	}
	_, err := p.amounts()
	return err
}
func (p SelectionPolicy) amounts() (map[domain.Address]*big.Int, error) {
	out := map[domain.Address]*big.Int{}
	for _, target := range p.Targets {
		token, err := domain.ParseAddress(target.Token)
		amount, ok := new(big.Int).SetString(target.Amount, 10)
		if err != nil || !ok || amount.Sign() <= 0 || amount.BitLen() > 256 || out[token] != nil {
			return nil, fmt.Errorf("invalid or duplicate discovery target")
		}
		out[token] = amount
	}
	return out, nil
}

// SelectMarkets admits whole funded cycles, preserving companion pools under
// caps. Activity/profit history break depth ties without comparing raw liquidity.
func SelectMarkets(ctx context.Context, pools []pool.State, activity map[domain.PoolKey]Activity, policy SelectionPolicy, maxPools, depth int) (Selection, error) {
	return SelectMarketsWithPinned(ctx, pools, activity, policy, maxPools, depth, nil)
}

// SelectMarketsWithPinned counts already-admitted pools against global caps.
// Existing readers retain their catalog; new cycles must fit in the remaining space.
func SelectMarketsWithPinned(ctx context.Context, pools []pool.State, activity map[domain.PoolKey]Activity, policy SelectionPolicy, maxPools, depth int, pinned []domain.PoolKey) (Selection, error) {
	return selectMarkets(ctx, nil, pools, activity, policy, maxPools, depth, pinned)
}

// selectMarkets enumerates candidate cycles through cache; a nil cache
// enumerates every candidate.
func selectMarkets(ctx context.Context, cache *cycleCache, pools []pool.State, activity map[domain.PoolKey]Activity, policy SelectionPolicy, maxPools, depth int, pinned []domain.PoolKey) (Selection, error) {
	if err := policy.Validate(); err != nil {
		return Selection{}, err
	}
	pinnedSet := map[domain.PoolKey]bool{}
	for _, key := range pinned {
		pinnedSet[key] = true
	}
	amounts, _ := policy.amounts()
	funding := map[domain.Address]bool{}
	for token := range amounts {
		funding[token] = true
	}
	pools = slices.Clone(pools)
	slices.SortFunc(pools, func(a, b pool.State) int { return domain.ComparePoolKeys(a.Key, b.Key) })
	// Propagate only a screening notional, through the lowest-impact available edge.
	// These spot conversions are not oracle prices or execution evidence.
	for pass := 0; pass < depth; pass++ {
		next := map[domain.Address]*big.Int{}
		best := map[domain.Address]uint64{}
		for _, p := range pools {
			r0, r1, ok := marketReserves(p)
			if !ok {
				continue
			}
			spot0, spot1, ok := marketSpotReserves(p)
			if !ok {
				continue
			}
			for side := 0; side < 2; side++ {
				in, out, rin := p.Token0, p.Token1, r0
				spotIn, spotOut := spot0, spot1
				if side == 1 {
					in, out, rin = out, in, r1
					spotIn, spotOut = spotOut, spotIn
				}
				amount := amounts[in]
				if amount == nil || amounts[out] != nil {
					continue
				}
				impact := reserveImpact(amount, rin)
				converted := new(big.Int).Div(new(big.Int).Mul(amount, spotOut), spotIn)
				if converted.Sign() == 0 || converted.BitLen() > 256 {
					continue
				}
				if _, exists := best[out]; !exists || impact < best[out] {
					best[out] = impact
					next[out] = converted
				}
			}
		}
		if len(next) == 0 {
			break
		}
		for token, amount := range next {
			amounts[token] = amount
		}
	}
	result := Selection{}
	eligible := []pool.State{}
	byKey := map[domain.PoolKey]Assessment{}
	for _, p := range pools {
		if err := ctx.Err(); err != nil {
			return Selection{}, err
		}
		row := Assessment{Pool: p.Key, Activity: ActivityFor(activity, p.Key), Reason: "unpriced"}
		r0, r1, ok := marketReserves(p)
		if ok {
			for side := 0; side < 2; side++ {
				token, reserve := p.Token0, r0
				if side == 1 {
					token, reserve = p.Token1, r1
				}
				amount := amounts[token]
				if amount == nil {
					continue
				}
				impact := reserveImpact(amount, reserve)
				if p.Key.Kind == domain.PoolKindVelodromeV2 && p.Velodrome != nil && p.Velodrome.Stable {
					impact = stableImpact(ctx, p, token, amount)
				}
				if row.TargetIn == nil || impact > row.ImpactBPS {
					row.TokenIn = token
					row.TargetIn = new(big.Int).Set(amount)
					row.ImpactBPS = impact
				}
			}
		}
		if row.TargetIn != nil {
			row.Reason = "no_funded_cycle"
			if row.ImpactBPS > policy.MaxSlippageBPS {
				row.Reason = "insufficient_depth"
			} else if row.Activity.Swaps < policy.MinSwaps && !pinnedSet[p.Key] {
				row.Reason = "inactive"
			} else {
				eligible = append(eligible, p)
			}
		}
		byKey[p.Key] = row
	}
	g, err := graph.Build(eligible)
	if err != nil {
		return Selection{}, err
	}
	if cache != nil {
		cache.sync(eligible, depth)
	}
	eligibleKeys := make([]domain.PoolKey, len(eligible))
	for i, p := range eligible {
		eligibleKeys[i] = p.Key
	}
	paths, capped, full, err := marketCycles(ctx, cache, g, depth, candidatePools(eligibleKeys, pinnedSet))
	if err != nil {
		return Selection{}, err
	}
	result.Truncated, result.CappedCandidates = full || capped > 0, capped
	type cycleScore struct {
		keys                        []domain.PoolKey
		impact, swaps, uses, routed uint64
	}
	states := map[domain.PoolKey]pool.State{}
	for _, p := range eligible {
		states[p.Key] = p
	}
	cycles := []cycleScore{}
	for _, cycle := range paths {
		current := cycle.Start
		funded := funding[current]
		score := cycleScore{keys: cycle.Pools}
		for _, key := range cycle.Pools {
			row := byKey[key]
			score.impact = max(score.impact, row.ImpactBPS)
			score.swaps += row.Activity.Swaps
			score.uses += row.Activity.ProfitableUses
			score.routed += row.Activity.MultiSwapUses
			p := states[key]
			if current == p.Token0 {
				current = p.Token1
			} else {
				current = p.Token0
			}
			funded = funded || funding[current]
		}
		if funded {
			cycles = append(cycles, score)
		}
	}
	slices.SortStableFunc(cycles, func(a, b cycleScore) int {
		if a.uses != b.uses {
			if a.uses > b.uses {
				return -1
			}
			return 1
		}
		if a.routed > b.routed {
			return -1
		}
		if a.routed < b.routed {
			return 1
		}
		if a.swaps != b.swaps {
			if a.swaps > b.swaps {
				return -1
			}
			return 1
		}
		if a.impact < b.impact {
			return -1
		}
		if a.impact > b.impact {
			return 1
		}
		return 0
	})
	admitted := map[domain.PoolKey]bool{}
	tokens := map[domain.Address]bool{}
	for _, key := range pinned {
		admitted[key] = true
	}
	for _, p := range pools {
		if pinnedSet[p.Key] {
			tokens[p.Token0] = true
			tokens[p.Token1] = true
		}
	}
	if len(admitted) > maxPools || len(tokens) > policy.MaxTokens {
		return Selection{}, fmt.Errorf("existing catalog exceeds market selection caps")
	}

	for _, cycle := range cycles {
		extra := 0
		newTokens := map[domain.Address]bool{}
		for _, key := range cycle.keys {
			if !admitted[key] {
				extra++
			}
			p := states[key]
			if !tokens[p.Token0] {
				newTokens[p.Token0] = true
			}
			if !tokens[p.Token1] {
				newTokens[p.Token1] = true
			}
		}
		if len(admitted)+extra > maxPools || len(tokens)+len(newTokens) > policy.MaxTokens {
			continue
		}
		for token := range newTokens {
			tokens[token] = true
		}
		for _, key := range cycle.keys {
			admitted[key] = true
		}
	}
	for _, p := range pools {
		row := byKey[p.Key]
		if admitted[p.Key] {
			row.Reason = "selected"
			if pinnedSet[p.Key] {
				row.Reason = "pinned"
			}
			result.Keys = append(result.Keys, p.Key)
		}
		result.Assessments = append(result.Assessments, row)
	}
	return result, nil
}

// Stable depth can differ greatly from its raw reserve ratio. This runs on the
// background discovery worker and needs no extra RPC reads or tick expansion.
func stableImpact(ctx context.Context, p pool.State, token domain.Address, amount *big.Int) uint64 {
	in, out, ok := marketSpotReserves(p)
	if !ok {
		return 10000
	}
	if token == p.Token1 {
		in, out = out, in
	}
	ideal := new(big.Int).Quo(new(big.Int).Mul(new(big.Int).Mul(amount, out), new(big.Int).SetUint64(uint64(1000000-p.Fee))), new(big.Int).Mul(in, big.NewInt(1000000)))
	actual, err := velodromev2.New().QuoteExactIn(ctx, p, pool.DeepState{}, token, amount)
	if err != nil || ideal.Sign() <= 0 || actual.Sign() <= 0 {
		return 10000
	}
	loss := new(big.Int).Sub(ideal, actual)
	if loss.Sign() <= 0 {
		return 0
	}
	bps := new(big.Int).Quo(new(big.Int).Mul(loss, big.NewInt(10000)), ideal)
	return min(uint64(10000), bps.Uint64())
}
func marketReserves(p pool.State) (*big.Int, *big.Int, bool) {
	if p.Key.Kind == domain.PoolKindV2 || p.Key.Kind == domain.PoolKindVelodromeV2 {
		return p.Reserve0, p.Reserve1, p.Reserve0 != nil && p.Reserve1 != nil && p.Reserve0.Sign() > 0 && p.Reserve1.Sign() > 0
	}
	if p.Liquidity == nil || p.SqrtPriceX96 == nil || p.Liquidity.Sign() <= 0 || p.SqrtPriceX96.Sign() <= 0 {
		return nil, nil, false
	}
	a := new(big.Int).Div(new(big.Int).Mul(p.Liquidity, poolmath.Q96), p.SqrtPriceX96)
	b := new(big.Int).Div(new(big.Int).Mul(p.Liquidity, p.SqrtPriceX96), poolmath.Q96)
	return a, b, a.Sign() > 0 && b.Sign() > 0
}
func reserveImpact(amount, reserve *big.Int) uint64 {
	value := new(big.Int).Div(new(big.Int).Mul(amount, big.NewInt(10000)), new(big.Int).Add(reserve, amount))
	return value.Uint64()
}

// PinnedMarketKeys preserves explicit seeds that successfully loaded, while
// counting them and the current catalog against the same pool/token limits.
func PinnedMarketKeys(known []domain.PoolKey, seeds []Spec, available []pool.State) []domain.PoolKey {
	keys := slices.Clone(known)
	existing := map[domain.PoolKey]bool{}
	for _, p := range available {
		existing[p.Key] = true
	}
	for _, seed := range seeds {
		if existing[seed.PoolKey()] {
			keys = append(keys, seed.PoolKey())
		}
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return slices.Compact(keys)
}

func marketSpotReserves(p pool.State) (*big.Int, *big.Int, bool) {
	model, err := dex.RankingModel(p)
	return model.Reserve0, model.Reserve1, err == nil
}
