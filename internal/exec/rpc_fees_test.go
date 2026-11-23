package exec

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func remoteReadyFixture(t *testing.T) *ReadyPlan {
	t.Helper()
	plan := readyFixture(t)
	e := &plan.Evidence
	e.FeeAccounting = "nitro-rpc-total"
	e.GasUsedForL1, e.PaidGasPrice, e.L1FeeWei, e.L2FeeWei = 0, nil, nil, nil
	sealed, err := SealReadyPlan(t.Context(), *plan)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func TestRemoteTotalFeeRoundtripAndAccountingIsolation(t *testing.T) {
	plan := remoteReadyFixture(t)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReadyPlan
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := ValidateForDispatch(t.Context(), &decoded, plan.Version, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS); err != nil {
		t.Fatal(err)
	}
	if err := (NitroFees{}).Validate(plan.Evidence); err == nil {
		t.Fatal("RPC evidence accepted as native split")
	}
	if err := (NitroRPCFees{}).Validate(readyFixture(t).Evidence); err == nil {
		t.Fatal("native split accepted as RPC total")
	}
	for name, change := range map[string]func(*ExecutionEvidence){
		"invented split": func(e *ExecutionEvidence) { e.L1FeeWei = big.NewInt(1) },
		"invented price": func(e *ExecutionEvidence) { e.PaidGasPrice = big.NewInt(1) },
		"over budget":    func(e *ExecutionEvidence) { e.TotalFeeWei = big.NewInt(200001) },
		"signed claim":   func(e *ExecutionEvidence) { e.Transaction = domain.Hash{1} },
		"gas overflow":   func(e *ExecutionEvidence) { e.GasUsed = e.Call.GasLimit + 1 },
		"missing charge": func(e *ExecutionEvidence) { e.TotalFeeWei = new(big.Int) },
	} {
		t.Run(name, func(t *testing.T) {
			e := remoteReadyFixture(t).Evidence
			change(&e)
			if err := (NitroRPCFees{}).Validate(e); err == nil {
				t.Fatal("invalid remote evidence accepted")
			}
		})
	}
}
