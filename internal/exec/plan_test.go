package exec

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func readyFixture(t *testing.T) *ReadyPlan {
	t.Helper()
	route := encodingPlan(t)
	route.Funding, route.Lender, route.LenderToken0, route.MaxLoanFee = FundingWallet, domain.PoolKey{}, false, new(big.Int)
	route.Asset, route.Steps[0].TokenIn, route.Steps[1].TokenOut = domain.Address{}, domain.Address{}, domain.Address{}
	route.Steps[0].Pool = route.Steps[1].Pool
	route.MinProfit, route.Deadline = big.NewInt(200_100), 1005
	parameters := callParameters()
	parameters.GasLimit, parameters.FeeCap, parameters.TipCap = 100_000, big.NewInt(2), new(big.Int)
	call, err := BuildExecutorCall(t.Context(), route, parameters)
	if err != nil {
		t.Fatal(err)
	}
	block := domain.BlockRef{Number: 7, Hash: domain.Hash{1}, Timestamp: 1000}
	evidence := ExecutionEvidence{Call: call, Owner: call.Sender, Context: ConfirmationContext{ChainID: 42161, Block: block, Timestamp: 1000,
		ArbOSVersion: 32, BlockGasLimit: 32_000_000, BaseFee: big.NewInt(2), L1UnitPrice: big.NewInt(100_000_000)},
		Profit: big.NewInt(1_000_000), LoanFee: new(big.Int), TotalFeeWei: big.NewInt(60_000), GasUsed: 30_000,
		GasUsedForL1: 5000, L1FeeWei: big.NewInt(10_000), L2FeeWei: big.NewInt(50_000), PaidGasPrice: big.NewInt(2),
		NonceAfter: call.Nonce + 1, L1FeeEstimated: true}
	conversion := FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1), AssetDecimals: 18}
	economics, err := CalculateEconomics(evidence, conversion, big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := SealReadyPlan(t.Context(), ReadyPlan{Route: route, Evidence: evidence, Economics: economics,
		Version:      domain.StateVersion{Source: domain.SourceStandard, Anchor: block, Epoch: 9, BlockNumber: 7, TxIndex: 1, PrefixHash: domain.Hash{2}},
		GraphVersion: 3, PolicyHash: domain.Hash{4}, CreatedUnixMS: 1_000_000, ExpiresUnixMS: 1_004_000})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestReadyPlanRoundtripAndDispatchGate(t *testing.T) {
	plan := readyFixture(t)
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReadyPlan
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := ValidateForDispatch(t.Context(), &decoded, plan.Version, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ReadyPlan){
		"prefix":     func(p *ReadyPlan) { p.Version.PrefixHash[0]++ },
		"epoch":      func(p *ReadyPlan) { p.Version.Epoch++ },
		"graph":      func(p *ReadyPlan) { p.GraphVersion++ },
		"policy":     func(p *ReadyPlan) { p.PolicyHash[0]++ },
		"calldata":   func(p *ReadyPlan) { p.Evidence.Call.Data[4]++ },
		"fee budget": func(p *ReadyPlan) { p.Economics.BudgetInAsset.Add(p.Economics.BudgetInAsset, big.NewInt(1)) },
		"profit":     func(p *ReadyPlan) { p.Evidence.Profit.Add(p.Evidence.Profit, big.NewInt(1)) },
	} {
		t.Run(name, func(t *testing.T) {
			modified := readyFixture(t)
			change(modified)
			if err := ValidateForDispatch(t.Context(), modified, plan.Version, 42161, plan.GraphVersion, plan.PolicyHash, plan.CreatedUnixMS); err == nil {
				t.Fatal("stale or modified plan passed dispatch gate")
			}
		})
	}
	if err := ValidateForDispatch(t.Context(), plan, plan.Version, 42161, plan.GraphVersion, plan.PolicyHash, plan.ExpiresUnixMS); err == nil {
		t.Fatal("expiry endpoint is inclusive")
	}
}

func TestReadyPlanRequiresConservativeProfitAndVerifiedPosition(t *testing.T) {
	for name, change := range map[string]func(*ReadyPlan){
		"working prefix":         func(p *ReadyPlan) { p.Version.Source = domain.SourceStandardPrefix },
		"unreplayed fast":        func(p *ReadyPlan) { p.Version.Source = domain.SourceFast },
		"wrong execution header": func(p *ReadyPlan) { p.Evidence.Context.Block.Hash[0]++ },
		"actual-only profit":     func(p *ReadyPlan) { p.Evidence.Profit.SetInt64(100_000) },
		"unprotected budget":     func(p *ReadyPlan) { p.Route.MinProfit.SetInt64(100) },
		"nonce":                  func(p *ReadyPlan) { p.Evidence.NonceAfter++ },
		"old execution context":  func(p *ReadyPlan) { p.Evidence.Context.Timestamp = 900 },
		"unsigned precise cost":  func(p *ReadyPlan) { p.Evidence.L1FeeEstimated = false },
	} {
		t.Run(name, func(t *testing.T) {
			plan := readyFixture(t)
			change(plan)
			if _, err := SealReadyPlan(t.Context(), *plan); err == nil {
				t.Fatal("invalid evidence was sealed ready")
			}
		})
	}
}

func TestEconomicsRoundsUpAndDoesNotChargeLoanTwice(t *testing.T) {
	evidence := readyFixture(t).Evidence
	evidence.LoanFee.SetInt64(5)
	conversion := FeeConversion{Numerator: big.NewInt(7), Denominator: big.NewInt(4)}
	result, err := CalculateEconomics(evidence, conversion, big.NewInt(1))
	if err != nil || result.ActualNetProfit.Int64() != 895_000 || result.BudgetNetProfit.Int64() != 650_000 {
		t.Fatalf("economics: %+v %v", result, err)
	}
	fee, err := FeeInAsset(big.NewInt(1), conversion)
	if err != nil || fee.Int64() != 2 {
		t.Fatalf("expense rounded down: %v %v", fee, err)
	}
	evidence.TotalFeeWei.SetInt64(200_001)
	if _, err := CalculateEconomics(evidence, conversion, big.NewInt(1)); err == nil {
		t.Fatal("fee above budget accepted")
	}
}

func TestReadyPlanDispatchRequiresSelectedChain(t *testing.T) {
	plan := *readyFixture(t)
	original := plan.ID
	plan.Evidence.Context.ChainID = 4663
	sealed, err := SealReadyPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ID == original {
		t.Fatal("plan hash did not bind chain identity")
	}
	if err := ValidateForDispatch(t.Context(), sealed, sealed.Version, 4663, sealed.GraphVersion, sealed.PolicyHash, sealed.CreatedUnixMS); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []uint64{0, 1, 42161} {
		if ValidateForDispatch(t.Context(), sealed, sealed.Version, wrong, sealed.GraphVersion, sealed.PolicyHash, sealed.CreatedUnixMS) == nil {
			t.Fatal("cross-chain plan accepted", wrong)
		}
	}
}
