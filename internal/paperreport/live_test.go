package paperreport

import (
	"bytes"
	"encoding/json"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"math/big"
	"testing"
)

func TestLiveReportExcludesShadowAndDeduplicatesCrashRecovery(t *testing.T) {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.Encode(map[string]any{"type": "reactive_intent", "mode": "shadow", "transaction": domain.Hash{2}})
	row := execution.Settlement{Hash: domain.Hash{1}, Intent: &execution.ReactiveIntent{Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}}, Asset: domain.Address{1}, Profit: big.NewInt(100), Success: true, FeeStatus: execution.FeesPending}
	// The feed booking precedes the paid row for the same hash.
	e.Encode(map[string]any{"type": "settlement", "receipt": row})
	row.Net, row.Fees, row.FeeStatus = big.NewInt(90), &domain.ReceiptFees{Total: big.NewInt(10)}, ""
	for range 2 {
		e.Encode(map[string]any{"type": "settlement", "receipt": row})
	}
	row.Hash = domain.Hash{3}
	row.Success = false
	row.Profit = new(big.Int)
	row.Net = big.NewInt(-10)
	e.Encode(map[string]any{"type": "settlement", "receipt": row})
	report, err := ReadLive(t.Context(), &buf, 100)
	if err != nil || report.Submitted != 0 || report.Settled != 2 || report.DuplicateSettlements != 1 || report.NativeFees.Int64() != 20 || report.Assets[0].Net.Int64() != 80 {
		t.Fatal(report, err)
	}
}

func TestLiveReportCancellationIsPaidCostNotArbitrageSuccess(t *testing.T) {
	var buf bytes.Buffer
	row := execution.Settlement{Hash: domain.Hash{1}, Intent: &execution.ReactiveIntent{Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}}, Asset: domain.Address{1}, Profit: new(big.Int), Net: big.NewInt(-30), Fees: &domain.ReceiptFees{Total: big.NewInt(30)}, Success: true, Cancelled: true}
	if err := json.NewEncoder(&buf).Encode(map[string]any{"type": "settlement", "receipt": row}); err != nil {
		t.Fatal(err)
	}
	report, err := ReadLive(t.Context(), &buf, 100)
	if err != nil || report.NativeFees.Int64() != 30 || report.Assets[0].Success != 0 || report.Assets[0].Cancelled != 1 || report.Assets[0].Net.Int64() != -30 {
		t.Fatal(report, err)
	}
}
