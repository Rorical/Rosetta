package tokenrisk

import (
	"encoding/json"
	"github.com/Rorical/Rosetta/internal/domain"
	"os"
	"strings"
	"testing"
)

func TestRecordedOnePercentTransferShortfall(t *testing.T) {
	raw, err := os.ReadFile("testdata/transfer-shortfall.json")
	if err != nil {
		t.Fatal(err)
	}
	var frame CallFrame
	if err = json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	executor, _ := domain.ParseAddress(frame.To)
	evidence := Detect(frame, domain.BlockRef{Number: 159002779, Hash: domain.Hash{1}}, executor)
	if len(evidence) != 1 {
		t.Fatalf("evidence: %+v", evidence)
	}
	e := evidence[0]
	if e.Token.Hex() != "0x74ccbe53f77b08632ce0cb91d3a545bf6b8e0979" || e.Requested.String() != "874780946346891277945" || e.Received.String() != "866033136883422365165" {
		t.Fatalf("unexpected shortfall: %+v", e)
	}
	// A failed transfer does not prove an actual deduction.
	var failTransfers func(*CallFrame)
	failTransfers = func(f *CallFrame) {
		if len(f.Input) >= 10 && f.Input[:10] == "0xa9059cbb" {
			f.Error = "reverted"
		}
		for i := range f.Calls {
			failTransfers(&f.Calls[i])
		}
	}
	failTransfers(&frame)
	if got := Detect(frame, domain.BlockRef{}, executor); len(got) != 0 {
		t.Fatalf("failed transfer classified: %+v", got)
	}
}

// Code inside the route (a hook) can read its own balance around a transfer
// to itself; the shortfall that leaves is staged, not a tax, and must not
// count. Neither does the executor's balance read by someone else.
func TestDetectCountsOnlyTheHoldersOwnReads(t *testing.T) {
	executor, hook, token := domain.Address{0xe}, domain.Address{0xa}, domain.Address{0x42}
	read := func(by, of domain.Address, amount string) CallFrame {
		return CallFrame{Type: "STATICCALL", From: by.Hex(), To: token.Hex(), Input: "0x70a08231" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(of.Hex()), "0x"), Output: "0x" + strings.Repeat("0", 64-len(amount)) + amount}
	}
	transfer := func(from, to domain.Address) CallFrame {
		return CallFrame{Type: "CALL", From: from.Hex(), To: token.Hex(), Input: "0xa9059cbb" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(to.Hex()), "0x") + strings.Repeat("0", 62) + "64"}
	}
	staged := CallFrame{Type: "CALL", From: executor.Hex(), To: hook.Hex(), Calls: []CallFrame{read(hook, hook, "0"), transfer(executor, hook), read(hook, hook, "10")}}
	foreign := CallFrame{Type: "CALL", From: executor.Hex(), To: hook.Hex(), Calls: []CallFrame{read(hook, executor, "0"), transfer(hook, executor), read(hook, executor, "10")}}
	for name, frame := range map[string]CallFrame{"staged": staged, "foreign": foreign} {
		if got := Detect(frame, domain.BlockRef{}, executor); len(got) != 0 {
			t.Errorf("%s shortfall attributed: %+v", name, got)
		}
	}
	own := CallFrame{Type: "CALL", To: executor.Hex(), Calls: []CallFrame{read(executor, executor, "0"), transfer(hook, executor), read(executor, executor, "10")}}
	if got := Detect(own, domain.BlockRef{}, executor); len(got) != 1 {
		t.Fatalf("executor's own shortfall missed: %+v", got)
	}
}
