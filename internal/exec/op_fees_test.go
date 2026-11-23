package exec

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/rlp"

	"github.com/Rorical/Rosetta/internal/domain"
)

func opReadyFixture(t *testing.T) *ReadyPlan {
	t.Helper()
	p := readyFixture(t)
	e := &p.Evidence
	e.FeeAccounting = "op-rpc-reserved"
	e.Context.Family, e.Context.ChainID, e.Context.ArbOSVersion, e.Context.L1BlockNumber = domain.FamilyOPStack, 8453, 0, 100
	e.GasUsedForL1 = 0
	e.L1FeeWei = big.NewInt(1000)
	e.L2FeeWei = big.NewInt(60000)
	e.OP = &OPFeeEvidence{TraceMode: "fee-free", OracleCodeHash: domain.Hash{9}, L1FeeUpperBound: big.NewInt(1000), OperatorFee: big.NewInt(50), OperatorFeeUpperBound: big.NewInt(200)}
	e.TotalFeeWei = big.NewInt(61050)
	var err error
	p.Economics, err = CalculateEconomics(*e, p.Economics.Conversion, big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	p.Route.MinProfit = big.NewInt(201300)
	e.Call, err = BuildExecutorCall(t.Context(), p.Route, e.Call.CallParameters)
	if err != nil {
		t.Fatal(err)
	}
	p, err = SealReadyPlan(t.Context(), *p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOPReadyPlanIncludesAllFeesAndRoundtrips(t *testing.T) {
	plan := opReadyFixture(t)
	if plan.Economics.NativeFeeBudget.Int64() != 201200 || plan.Economics.ActualFeeInAsset.Int64() != 61050 {
		t.Fatal("OP costs omitted", plan.Economics)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReadyPlan
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := ValidateForDispatch(t.Context(), &decoded, plan.Version, 8453, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS); err != nil {
		t.Fatal(err)
	}
	if err := (NitroFees{}).Validate(plan.Evidence); err == nil {
		t.Fatal("OP passed Nitro accounting")
	}
	for name, change := range map[string]func(*ExecutionEvidence){
		"operator over bound":    func(e *ExecutionEvidence) { e.OP.OperatorFee = big.NewInt(201) },
		"missing operator bound": func(e *ExecutionEvidence) { e.OP.OperatorFeeUpperBound = nil },
		"wrong data reserve":     func(e *ExecutionEvidence) { e.L1FeeWei = big.NewInt(999) },
		"missing data total":     func(e *ExecutionEvidence) { e.TotalFeeWei = big.NewInt(60050) },
		"wrong price":            func(e *ExecutionEvidence) { e.PaidGasPrice = big.NewInt(1) },
		"Nitro context":          func(e *ExecutionEvidence) { e.Context.ArbOSVersion = 32 },
		"signed claim":           func(e *ExecutionEvidence) { e.L1FeeEstimated = false },
	} {
		t.Run(name, func(t *testing.T) {
			e := opReadyFixture(t).Evidence
			change(&e)
			if err := (OPFees{}).Validate(e); err == nil {
				t.Fatal("invalid OP fees accepted")
			}
		})
	}
}

func TestUnsignedFeePayloadBindsEnvelopeAndABISize(t *testing.T) {
	p := opReadyFixture(t)
	raw, err := UnsignedDynamicFeeTx(p.Evidence.Call, 8453)
	if err != nil {
		t.Fatal(err)
	}
	var fields []rlp.RawValue
	if raw[0] != 2 {
		t.Fatal("not a typed fee-market transaction")
	}
	if err := rlp.DecodeBytes(raw[1:], &fields); err != nil || len(fields) != 9 {
		t.Fatal("wrong signing payload", err)
	}
	var id uint64
	if err := rlp.DecodeBytes(fields[0], &id); err != nil || id != 8453 {
		t.Fatal("chain identity not encoded", err)
	}
	p.Route.MinProfit = new(big.Int).Lsh(big.NewInt(1), 250)
	changed, err := BuildExecutorCall(t.Context(), p.Route, p.Evidence.Call.CallParameters)
	if err != nil {
		t.Fatal(err)
	}
	second, err := UnsignedDynamicFeeTx(changed, 8453)
	if err != nil || len(second) != len(raw) {
		t.Fatal("ABI minProfit changed the quoted size", err)
	}
	bound, err := OPL1SizeBound(len(raw))
	if err != nil || bound <= uint64(2*(len(raw)+68)) {
		t.Fatal("signed size/control overhead omitted", bound, err)
	}
}
