package rpc

import (
	"encoding/json"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestReceiptPaidFees(t *testing.T) {
	tests := []struct {
		name, raw string
		family    domain.ChainFamily
		total     string
		fail      bool
	}{
		{"OP real receipt zero operator", `{"gasUsed":"0x2b727","effectiveGasPrice":"0x30e5a","l1Fee":"0x224c6d88","daFootprintGasScalar":"0x190"}`, domain.FamilyOPStack, "", false},
		{"Isthmus", `{"gasUsed":"0x186a0","effectiveGasPrice":"0x2","l1Fee":"0xa","operatorFeeScalar":"0xf","operatorFeeConstant":"0x7"}`, domain.FamilyOPStack, "200018", false},
		{"Jovian", `{"gasUsed":"0x186a0","effectiveGasPrice":"0x2","l1Fee":"0xa","operatorFeeScalar":"0xf","operatorFeeConstant":"0x7","daFootprintGasScalar":"0x190"}`, domain.FamilyOPStack, "150200017", false},
		{"Nitro all in", `{"gasUsed":"0x186a0","effectiveGasPrice":"0x2","l1Fee":"0xa"}`, domain.FamilyNitro, "200000", false},
		{"missing L1", `{"gasUsed":"0x1","effectiveGasPrice":"0x2"}`, domain.FamilyOPStack, "nil", false},
		{"partial operator", `{"gasUsed":"0x1","effectiveGasPrice":"0x2","l1Fee":"0x0","operatorFeeScalar":"0x1"}`, domain.FamilyOPStack, "", true},
		{"malformed", `{"gasUsed":"0x1","effectiveGasPrice":"-2"}`, domain.FamilyNitro, "", true},
		{"deposit", `{"type":"0x7e","gasUsed":"0x1","effectiveGasPrice":"0x0"}`, domain.FamilyOPStack, "nil", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p receiptFeePayload
			if err := json.Unmarshal([]byte(tt.raw), &p); err != nil {
				t.Fatal(err)
			}
			got, err := p.fees(tt.family)
			if (err != nil) != tt.fail {
				t.Fatal(err)
			}
			if tt.fail {
				return
			}
			if tt.total == "nil" {
				if got != nil {
					t.Fatal(got)
				}
				return
			}
			if got == nil {
				t.Fatal("missing paid fees")
			}
			if tt.total != "" && got.Total.String() != tt.total {
				t.Fatal(got.Total)
			}
			if tt.name == "OP real receipt zero operator" && got.Total.String() != "36217418558" {
				t.Fatal(got.Total)
			}
		})
	}
}

func TestExecutionReceiptsPreserveBoundPaidFees(t *testing.T) {
	block := ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, Transactions: []domain.Hash{{20}}}
	raw := map[string]any{"transactionHash": domain.Hash{20}.Hex(), "blockHash": block.Ref.Hash.Hex(), "blockNumber": "0xa", "transactionIndex": "0x0", "status": "0x1", "logs": []any{}, "gasUsed": "0x186a0", "effectiveGasPrice": "0x2", "l1Fee": "0xa"}
	got, err := executionServer(t, []any{raw}).ExecutionReceipts(t.Context(), block, domain.FamilyOPStack)
	if err != nil || len(got) != 1 || got[0].Fees == nil || got[0].Fees.Total.String() != "200010" {
		t.Fatal(got, err)
	}
	raw["blockHash"] = domain.Hash{9}.Hex()
	if _, err = executionServer(t, []any{raw}).ExecutionReceipts(t.Context(), block, domain.FamilyOPStack); err == nil {
		t.Fatal("fee metadata accepted detached receipt")
	}
}
