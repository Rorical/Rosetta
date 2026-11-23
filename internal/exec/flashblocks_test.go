package exec

import (
	"encoding/json"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func flashReadyFixture(t *testing.T) *ReadyPlan {
	t.Helper()
	plan := opReadyFixture(t)
	plan.Version.Source = domain.SourceFast
	plan.Version.BlockNumber = plan.Version.Anchor.Number + 1
	plan.Version.TxIndex = 2
	header := plan.Evidence.Context.Block
	header.ParentHash, header.Hash, header.Number = plan.Version.Anchor.Hash, domain.Hash{10}, plan.Version.BlockNumber
	plan.Evidence.Context.Block = header
	plan.Flashblocks = &FlashblocksEvidence{Version: plan.Version, PayloadID: "0x0102030405060708", Index: 1, Transactions: 3, Source: domain.Hash{11}, ContextHash: header.Hash, RawBlockHash: domain.Hash{12}, OverlayHash: domain.Hash{13}, BlockGasLeft: 1_000_000}
	ready, err := SealReadyPlan(t.Context(), *plan)
	if err != nil {
		t.Fatal(err)
	}
	return ready
}
func TestFlashblocksReadyDispatchRequiresCurrentPrefix(t *testing.T) {
	plan := flashReadyFixture(t)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReadyPlan
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	current := FlashblocksDispatchContext{Version: plan.Version, PayloadID: plan.Flashblocks.PayloadID, Index: 1, Source: plan.Flashblocks.Source}
	check := func(current FlashblocksDispatchContext) error {
		return ValidateFlashblocksForDispatch(t.Context(), &decoded, current, 8453, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS)
	}
	if err := check(current); err != nil {
		t.Fatal(err)
	}
	if err := ValidateForDispatch(t.Context(), &decoded, plan.Version, 8453, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS); err == nil {
		t.Fatal("canonical dispatch admitted Flashblocks plan")
	}
	for _, field := range []string{"index", "payload", "source", "prefix", "epoch"} {
		bad := current
		switch field {
		case "index":
			bad.Index++
		case "payload":
			bad.PayloadID = "0x1111111111111111"
		case "source":
			bad.Source[0]++
		case "prefix":
			bad.Version.PrefixHash[0]++
		case "epoch":
			bad.Version.Epoch++
		}
		if err := check(bad); err == nil {
			t.Fatal("stale stream accepted", field)
		}
	}
	decoded.Flashblocks.OverlayHash[0]++
	if err := check(current); err == nil {
		t.Fatal("altered replay commitment accepted")
	}
	for _, field := range []string{"nonce prefix", "gas", "header", "Nitro proof", "state source"} {
		bad := flashReadyFixture(t)
		switch field {
		case "nonce prefix":
			bad.Flashblocks.Transactions++
		case "gas":
			bad.Flashblocks.BlockGasLeft = 0
		case "header":
			bad.Evidence.Context.Block.ParentHash = domain.Hash{99}
		case "Nitro proof":
			bad.FastReplay = &FastReplayEvidence{}
		case "state source":
			bad.Flashblocks.StateSource = "pending"
		}
		if _, err := SealReadyPlan(t.Context(), *bad); err == nil {
			t.Fatal("inconsistent replay sealed", field)
		}
	}
}

func TestFlashblocksCachedStateEvidenceRoundTrip(t *testing.T) {
	plan := flashReadyFixture(t)
	plan.Flashblocks.StateSource = "rpc-cache-v1"
	sealed, err := SealReadyPlan(t.Context(), *plan)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReadyPlan
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	current := FlashblocksDispatchContext{Version: plan.Version, PayloadID: plan.Flashblocks.PayloadID, Index: plan.Flashblocks.Index, Source: plan.Flashblocks.Source}
	if err := ValidateFlashblocksForDispatch(t.Context(), &decoded, current, 8453, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS); err != nil {
		t.Fatal(err)
	}
}
