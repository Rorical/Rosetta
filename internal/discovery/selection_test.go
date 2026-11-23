package discovery

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func marketState(address byte, a, b domain.Address, x, y int64) pool.State {
	return pool.State{Key: domain.PoolKey{Address: domain.Address{address}, Kind: domain.PoolKindV2}, Token0: a, Token1: b, Reserve0: big.NewInt(x), Reserve1: big.NewInt(y), Fee: 3000}
}
func TestSelectionKeepsWholeCyclesAndNormalizesTokenUnits(t *testing.T) {
	a, b, c := domain.Address{1}, domain.Address{2}, domain.Address{3}
	// A six-decimal/18-decimal pair and its alternative have equal economic depth.
	pools := []pool.State{marketState(10, a, b, 1000000, 1000000000000000000), marketState(11, a, b, 1000000, 1000000000000000000), marketState(12, a, c, 1000000, 1000000)}
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: a.Hex(), Amount: "1000"}}, MaxTokens: 2, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 32}
	selected, err := SelectMarkets(context.Background(), pools, nil, policy, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Keys) != 2 || selected.Keys[0] != pools[0].Key || selected.Keys[1] != pools[1].Key {
		t.Fatal("parallel funded cycle lost", selected)
	}
	for _, row := range selected.Assessments {
		if row.Pool == pools[2].Key && row.Reason != "no_funded_cycle" {
			t.Fatal("isolated market admitted", row)
		}
	}
	policy.MaxTokens = 3
	selected, err = SelectMarkets(t.Context(), pools, nil, policy, 1, 3)
	if err != nil || len(selected.Keys) != 0 {
		t.Fatal("cap split a cycle", selected, err)
	}
}
func TestSelectionActivityAndDepthCannotBeReplacedByRawLiquidity(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	pools := []pool.State{marketState(10, a, b, 1000000, 1000000), marketState(11, a, b, 1000000, 1000000), marketState(12, a, b, 1, 1000000000000000000)}
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: a.Hex(), Amount: "1000"}}, MaxTokens: 2, MaxSlippageBPS: 100, MinSwaps: 1, ActivityWindow: 100, ProbeLimit: 32}
	activity := map[domain.PoolKey]Activity{pools[0].Key: {Swaps: 2}, pools[1].Key: {Swaps: 1}, pools[2].Key: {Swaps: 100}}
	result, err := SelectMarkets(t.Context(), pools, activity, policy, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Keys) != 2 || result.Keys[1] != pools[1].Key {
		t.Fatal("unusable high-unit market displaced good cycle", result)
	}
}

func TestPinnedQuietPoolCanCompleteNewActiveFundedCycle(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	pools := []pool.State{marketState(10, a, b, 1000000, 1000000), marketState(11, a, b, 1000000, 1000000)}
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: a.Hex(), Amount: "1000"}}, MaxTokens: 2, MaxSlippageBPS: 100, MinSwaps: 2, ActivityWindow: 100, ProbeLimit: 4}
	result, err := SelectMarketsWithPinned(t.Context(), pools, map[domain.PoolKey]Activity{pools[1].Key: {Swaps: 3}}, policy, 2, 2, []domain.PoolKey{pools[0].Key})
	if err != nil || len(result.Keys) != 2 {
		t.Fatal("quiet admitted leg blocked an active alternative", result, err)
	}
}

func TestSelectionKeepsLateParallelMarketsWhenDeepEnumerationTruncates(t *testing.T) {
	tokens := make([]domain.Address, 8)
	for i := range tokens {
		tokens[i] = domain.Address{byte(i + 1)}
	}
	var pools []pool.State
	for i := range tokens {
		for j := i + 1; j < len(tokens); j++ {
			for range 3 {
				pools = append(pools, marketState(byte(len(pools)+1), tokens[i], tokens[j], 1000000, 1000000))
			}
		}
	}
	late := domain.Address{99}
	pools = append(pools, marketState(240, tokens[0], late, 1000000, 1000000), marketState(241, tokens[0], late, 1000000, 1000000))
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: tokens[0].Hex(), Amount: "1"}}, MaxTokens: 16, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 128}
	result, err := SelectMarkets(t.Context(), pools, nil, policy, len(pools), 6)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []domain.PoolKey{pools[len(pools)-2].Key, pools[len(pools)-1].Key} {
		found := false
		for _, got := range result.Keys {
			if got == key {
				found = true
			}
		}
		if !found {
			t.Fatal("deep prefix hid a directly funded two-pool market", key)
		}
	}
}

func TestSelectionPreservesLateFiveHopMarketBeyondDenseCyclePrefix(t *testing.T) {
	var pools []pool.State
	for a := byte(1); a <= 7; a++ {
		for b := a + 1; b <= 7; b++ {
			for range 3 {
				pools = append(pools, marketState(byte(len(pools)+1), domain.Address{a}, domain.Address{b}, 1000000, 1000000))
			}
		}
	}
	var ring []domain.PoolKey
	for index := byte(0); index < 5; index++ {
		from, to := byte(90+index), byte(91+index)
		if index == 0 {
			from = 1
		}
		if index == 4 {
			to = 1
		}
		p := marketState(240+index, domain.Address{from}, domain.Address{to}, 1000000, 1000000)
		pools, ring = append(pools, p), append(ring, p.Key)
	}
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: (domain.Address{1}).Hex(), Amount: "1"}}, MaxTokens: 16, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 128}
	result, err := SelectMarkets(t.Context(), pools, nil, policy, len(pools), 6)
	if err != nil || !result.Truncated {
		t.Fatal("fixture did not truncate", result, err)
	}
	for _, key := range ring {
		found := false
		for _, selected := range result.Keys {
			found = found || selected == key
		}
		if !found {
			t.Fatal("dense early markets hid a funded five-hop companion", key)
		}
	}
}

// 150 admitted parallel pools on one pair give over 20000 directed two-pool
// cycles, the old global table budget. A new candidate on another pair, whose
// only cycle sorts after that core, must still be judged.
func TestSelectionEnumeratesCyclesThroughEachCandidateBehindDenseCatalog(t *testing.T) {
	a, b, c := domain.Address{1}, domain.Address{2}, domain.Address{3}
	state := func(hi, lo byte, x, y domain.Address) pool.State {
		return pool.State{Key: domain.PoolKey{Address: domain.Address{hi, lo}, Kind: domain.PoolKindV2}, Token0: x, Token1: y, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(1000000), Fee: 3000}
	}
	var pools []pool.State
	var pinned []domain.PoolKey
	for i := 0; i < 150; i++ {
		p := state(0x10, byte(i), a, b)
		pools, pinned = append(pools, p), append(pinned, p.Key)
	}
	companion := state(0xfe, 0, a, c)
	candidate := state(0xff, 0xff, a, c)
	pools, pinned = append(pools, companion, candidate), append(pinned, companion.Key)
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: a.Hex(), Amount: "1000"}}, MaxTokens: 3, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 32}
	result, err := SelectMarketsWithPinned(t.Context(), pools, nil, policy, 512, 3, pinned)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range result.Keys {
		if key == candidate.Key {
			return
		}
	}
	t.Fatal("dense admitted catalog hid the candidate's cycles")
}

func TestSelectionDepthAdmitsLongerCyclesThanTheSearchTable(t *testing.T) {
	var pools []pool.State
	for index := byte(0); index < 5; index++ {
		from, to := domain.Address{90 + index}, domain.Address{91 + index}
		if index == 0 {
			from = domain.Address{1}
		}
		if index == 4 {
			to = domain.Address{1}
		}
		pools = append(pools, marketState(240+index, from, to, 1000000, 1000000))
	}
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: (domain.Address{1}).Hex(), Amount: "1"}}, MaxTokens: 16, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 8}
	for _, tc := range []struct {
		maxDepth, admitted int
	}{{0, 0}, {5, 5}} {
		policy.MaxDepth = tc.maxDepth
		result, err := SelectMarkets(t.Context(), pools, nil, policy, 16, policy.Depth(4))
		if err != nil || len(result.Keys) != tc.admitted {
			t.Fatal("max_depth", tc.maxDepth, "admitted", len(result.Keys), err)
		}
	}
	policy.MaxDepth = 9
	if policy.Validate() == nil {
		t.Fatal("admission depth beyond the explorer accepted")
	}
}
