package exec

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestPackedEncodingMatchesCastAndSolidityVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/packed_plan.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct{ Calldata, Hash string }
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodePackedExecutorPlan(t.Context(), encodingPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if "0x"+hex.EncodeToString(encoded) != vector.Calldata || keccak(encoded).Hex() != vector.Hash {
		t.Fatalf("packed vector differs: %x", encoded)
	}
}

func TestPackedEncodingPreservesWidthsAndOmitsDefaultLimit(t *testing.T) {
	plan := encodingPlan(t)
	plan.Funding, plan.Lender, plan.LenderToken0, plan.MaxLoanFee = FundingV3FlashSwap, domain.PoolKey{}, false, new(big.Int)
	plan.Amount = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	plan.Deadline = ^uint64(0)
	plan.Steps[1].SqrtPriceLimitX96 = nil
	plan.Steps[1].TickSpacing = -60
	encoded, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// 95-byte header and two 80-byte steps, padded to 256 after the 68-byte ABI head.
	if len(encoded) != 68+256 || encoded[69] != byte(FundingV3FlashSwap) || new(big.Int).SetBytes(encoded[36:68]).Int64() != 255 {
		t.Fatal("unexpected compact two-hop length or funding")
	}
	payload := encoded[68:]
	if new(big.Int).SetBytes(payload[23:55]).Cmp(plan.Amount) != 0 || new(big.Int).SetBytes(payload[87:95]).Uint64() != plan.Deadline {
		t.Fatal("header integer truncated")
	}
	second := payload[95+80:]
	if second[0] != byte(domain.PoolKindV4) || hex.EncodeToString(second[44:47]) != "ffffc4" || second[79] != 0 {
		t.Fatal("V4 step kind, signed tick spacing or default limit flag differs")
	}
	plan.Steps[1].SqrtPriceLimitX96 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	withLimit, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	limit := withLimit[68+95+80+80 : 68+95+80+100]
	if withLimit[68+95+80+79] != 1 || new(big.Int).SetBytes(limit).Cmp(plan.Steps[1].SqrtPriceLimitX96) != 0 {
		t.Fatal("price limit truncated")
	}
}

// checkedEncodingPlan is encodingPlan with a bound on each step; the second
// step also keeps its explicit price limit, which precedes the bound.
func checkedEncodingPlan(t *testing.T) ExecutorPlan {
	plan := encodingPlan(t)
	plan.Steps[0].PriceBound = new(big.Int).Lsh(big.NewInt(1), 96)
	plan.Steps[1].PriceBound = big.NewInt(4295128741)
	return plan
}

func TestPackedPriceBoundsUseVersionThreeAndMatchSolidity(t *testing.T) {
	encoded, err := EncodePackedExecutorPlan(t.Context(), checkedEncodingPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	// Shared with ExecutorEncodingTest, which builds the same bytes in Solidity.
	if keccak(encoded).Hex() != "0x14cbfed9cae1acade61bece63c49dd34ec767766fc5497b49fdafea27ab84d29" {
		t.Fatalf("checked vector differs: %x %s", encoded, keccak(encoded).Hex())
	}
	payload := encoded[68:]
	first, second := payload[148:248], payload[248:]
	if new(big.Int).SetBytes(encoded[36:68]).Int64() != 368 || payload[0] != 3 || first[79] != 2 || second[79] != 3 {
		t.Fatal("unexpected version, length or step flags")
	}
	if new(big.Int).SetBytes(first[80:100]).Cmp(new(big.Int).Lsh(big.NewInt(1), 96)) != 0 ||
		new(big.Int).SetBytes(second[80:100]).Int64() != 4295128740 || new(big.Int).SetBytes(second[100:120]).Int64() != 4295128741 {
		t.Fatal("price limit or bound misplaced")
	}
}

func TestPriceBoundsRejectedOutsideTheirFieldAndTheABIEntry(t *testing.T) {
	for _, bound := range []*big.Int{new(big.Int), new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(-1)} {
		plan := checkedEncodingPlan(t)
		plan.Steps[1].PriceBound = bound
		if _, err := EncodePackedExecutorPlan(t.Context(), plan); err == nil {
			t.Fatalf("bound %s accepted", bound)
		}
	}
	if _, err := EncodeExecutorPlan(t.Context(), checkedEncodingPlan(t)); err == nil {
		t.Fatal("ABI entry accepted price bounds it cannot carry")
	}
}

// Version 4 carries the cumulative precheck's minimum rate after the header;
// the steps' prices are quote references. Shared with ExecutorEncodingTest.
func TestPackedMinRateUsesVersionFourAndMatchesSolidity(t *testing.T) {
	plan := checkedEncodingPlan(t)
	plan.MinRate = big.NewInt(999e15)
	encoded, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if keccak(encoded).Hex() != "0x044cfc49988b1d9f072da379747b4116f0d2b2fa7f9c64844ef67417032662b7" {
		t.Fatalf("cumulative vector differs: %s", keccak(encoded).Hex())
	}
	payload := encoded[68:]
	// The lender follows the rate; every later field moves by 8 bytes.
	if payload[0] != 4 || new(big.Int).SetBytes(payload[95:103]).Cmp(plan.MinRate) != 0 || payload[103] != 0x66 || payload[156+79] != 2 {
		t.Fatal("unexpected version, rate or offsets")
	}
	if len(encoded) > PackedCalldataBound(len(plan.Steps)) {
		t.Fatal("calldata bound too small for version 4")
	}
	for _, rate := range []*big.Int{new(big.Int), big.NewInt(1e18), big.NewInt(-1)} {
		plan.MinRate = rate
		if _, err := EncodePackedExecutorPlan(t.Context(), plan); err == nil {
			t.Fatalf("min rate %s accepted", rate)
		}
	}
	plan.MinRate = big.NewInt(999e15)
	if _, err := EncodeExecutorPlan(t.Context(), plan); err == nil {
		t.Fatal("ABI entry accepted a min rate it cannot carry")
	}
}
