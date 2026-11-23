package exec

import (
	"encoding/json"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestFastPlanRequiresReplayAndLiveStreamObservations(t *testing.T) {
	plan := readyFixture(t)
	plan.Version.Source = domain.SourceFast
	plan.Version.Anchor.Number--
	plan.FastReplay = &FastReplayEvidence{Version: plan.Version, ContextSource: domain.Hash{5}, ContextHash: domain.Hash{6},
		StateRoot: domain.Hash{7}, HeaderHash: plan.Evidence.Context.Block.Hash, Transactions: 2, Generation: 3, ReceiptRevision: 9, BlockGasLeft: 1_000_000}
	plan, err := SealReadyPlan(t.Context(), *plan)
	if err != nil {
		t.Fatal(err)
	}
	current := FastDispatchContext{Version: plan.Version, ContextSource: plan.FastReplay.ContextSource, Generation: 3, ReceiptRevision: 9}
	if err := ValidateFastForDispatch(t.Context(), plan, current, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS); err != nil {
		t.Fatal(err)
	}
	if ValidateForDispatch(t.Context(), plan, plan.Version, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS) == nil {
		t.Fatal("ordinary dispatch bypassed live Fast observations")
	}
	for _, change := range []func(*FastDispatchContext){
		func(c *FastDispatchContext) { c.Generation++ }, func(c *FastDispatchContext) { c.ReceiptRevision++ },
		func(c *FastDispatchContext) { c.ContextSource[0]++ }, func(c *FastDispatchContext) { c.Version.Epoch++ },
	} {
		stale := current
		change(&stale)
		if ValidateFastForDispatch(t.Context(), plan, stale, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS) == nil {
			t.Fatal("stale Fast plan was dispatched")
		}
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReadyPlan
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFastForDispatch(t.Context(), &decoded, current, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS); err != nil {
		t.Fatal(err)
	}
	decoded.FastReplay.ContextHash[0]++
	if ValidateFastForDispatch(t.Context(), &decoded, current, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS) == nil {
		t.Fatal("modified replay evidence passed content gate")
	}
}
