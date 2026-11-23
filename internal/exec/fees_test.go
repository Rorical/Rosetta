package exec

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A contract probe, not an implementation of OP's evolving fee rules.
type feeModelProbe struct {
	family     domain.ChainFamily
	extra      int64
	validation error
	calls      int
}

func (f *feeModelProbe) Family() domain.ChainFamily { return f.family }
func (f *feeModelProbe) Budget(e ExecutionEvidence) (*big.Int, error) {
	return new(big.Int).Add(new(big.Int).Mul(new(big.Int).SetUint64(e.Call.GasLimit), e.Call.FeeCap), big.NewInt(f.extra)), nil
}
func (f *feeModelProbe) Validate(ExecutionEvidence) error { f.calls++; return f.validation }

func TestFeeBoundaryRequiresMatchingFamilyAndUsesAdditionalBudget(t *testing.T) {
	plan := readyFixture(t)
	plan.Evidence.Context.Family = domain.FamilyOPStack
	plan.Evidence.Context.ArbOSVersion = 0
	if _, err := SealReadyPlan(t.Context(), *plan); err == nil {
		t.Fatal("OP evidence used implicit Nitro validator")
	}
	fees := &feeModelProbe{family: domain.FamilyOPStack, extra: 500}
	economics, err := CalculateEconomics(plan.Evidence, plan.Economics.Conversion, plan.Economics.MinimumNetProfit, fees)
	if err != nil || economics.NativeFeeBudget.Int64() != 200_500 {
		t.Fatal("additional backend fee omitted", economics, err)
	}
	plan.Economics = economics
	plan.Route.MinProfit = big.NewInt(200_600)
	plan.Evidence.Call, err = BuildExecutorCall(t.Context(), plan.Route, plan.Evidence.Call.CallParameters)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealReadyPlan(t.Context(), *plan, fees)
	if err != nil || fees.calls != 1 {
		t.Fatal("backend validation not used", err, fees.calls)
	}
	if err := ValidateForDispatch(t.Context(), sealed, plan.Version, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS, fees); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("native fee proof rejected")
	fees.validation = rejected
	if err := ValidateForDispatch(t.Context(), sealed, plan.Version, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS, fees); !errors.Is(err, rejected) {
		t.Fatal("dispatch skipped native validation", err)
	}
	fees.family = domain.FamilyNitro
	if _, err := CalculateEconomics(plan.Evidence, plan.Economics.Conversion, big.NewInt(1), fees); err == nil {
		t.Fatal("wrong family supplied budget")
	}
}
