package planning

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	execution "github.com/Rorical/Rosetta/internal/exec"
)

func TestFlashAccountingPolicyFundingConstraints(t *testing.T) {
	address := func(digit string) string { return "0x" + strings.Repeat(digit, 40) }
	for _, test := range []struct {
		funding string
		mode    execution.Funding
		name    string
		valid   bool
	}{
		{"v3_flash_swap", execution.FundingV3FlashSwap, "ERC20", true},
		{"v3_flash_swap", execution.FundingV3FlashSwap, "lender", false},
		{"v3_flash_swap", execution.FundingV3FlashSwap, "loan fee", false},
		{"v3_flash_swap", execution.FundingV3FlashSwap, "native", false},
		{"v4_flash", execution.FundingV4Flash, "ERC20", true},
		{"v4_flash", execution.FundingV4Flash, "native", true},
		{"v4_flash", execution.FundingV4Flash, "lender", false},
		{"v4_flash", execution.FundingV4Flash, "loan fee", false},
	} {
		t.Run(test.funding+"/"+test.name, func(t *testing.T) {
			asset := map[string]any{"token": address("4"), "funding": test.funding, "max_loan_fee": "0",
				"min_net_profit": "1", "usd_feed": address("5"), "max_price_age_seconds": 60}
			switch test.name {
			case "lender":
				asset["lender"] = address("6")
			case "loan fee":
				asset["max_loan_fee"] = "1"
			case "native":
				asset["token"] = address("0")
			}
			document := map[string]any{"sender": address("1"), "executor": address("2"), "code_hash": "0x" + strings.Repeat("3", 64),
				"gas_limit": 1_000_000, "max_fee_per_gas_wei": "100", "tip_per_gas_wei": "2", "max_total_fee_wei": "100000000",
				"fee_multiplier_bps": 20_000, "slippage_bps": 50, "valid_for_seconds": 10, "max_candidates": 8, "timeout_ms": 100,
				"native_usd_feed": address("7"), "sequencer_feed": address("8"), "sequencer_grace_seconds": 3600, "assets": []any{asset}}
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "policy.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			policy, err := LoadPolicy(path)
			if !test.valid {
				if err == nil {
					t.Fatal("invalid flash accounting policy accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, configured := range policy.Assets {
				if configured.Funding != test.mode || configured.MaxLoanFee.Sign() != 0 {
					t.Fatal("flash accounting funding not preserved")
				}
			}
		})
	}
}
