package exec

import (
	"encoding/hex"
	"encoding/json"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFailedLiveReceiptLearnsTransferRiskWithoutLosingSettlement(t *testing.T) {
	raw, err := os.ReadFile("../tokenrisk/testdata/transfer-shortfall.json")
	if err != nil {
		t.Fatal(err)
	}
	var frame tokenrisk.CallFrame
	if err = json.Unmarshal(raw, &frame); err != nil {
		t.Fatal(err)
	}
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     any    `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "debug_traceTransaction" {
			t.Errorf("unexpected RPC %s", req.Method)
		}
		if unavailable.Load() {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "disabled"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": frame})
	}))
	defer server.Close()
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	risks, err := tokenrisk.Open("", chain)
	if err != nil {
		t.Fatal(err)
	}
	defer risks.Close()
	intent := ReactiveIntent{}
	intent.Call.Sender, _ = domain.ParseAddress(frame.From)
	intent.Call.Executor, _ = domain.ParseAddress(frame.To)
	intent.Call.Data, _ = hex.DecodeString(frame.Input[2:])
	n := RPCDispatchNetwork{Read: rpc.NewClient(server.URL, rpc.Options{}), Chain: chain, TokenRisks: risks}
	settled := &Settlement{Hash: domain.Hash{1}, Block: domain.BlockRef{Number: 159002779, Hash: domain.Hash{2}}}
	if err = n.observeFailedTransfer(t.Context(), intent, settled); err != nil {
		t.Fatal(err)
	}
	token, _ := domain.ParseAddress("0x74ccbe53f77b08632ce0cb91d3a545bf6b8e0979")
	if settled.TransferDiagnosis != "transfer_shortfall_recorded" || risks.Check(token) == nil {
		t.Fatal(settled.TransferDiagnosis)
	}
	unavailable.Store(true)
	if err = n.observeFailedTransfer(t.Context(), intent, settled); err != nil || settled.TransferDiagnosis != "trace_unavailable" {
		t.Fatal("diagnostic outage blocked paid settlement", err)
	}
}

func TestPoolMovedDiagnosisChecksRootIdentityAndWorksWithoutTokenRegistry(t *testing.T) {
	intent := ReactiveIntent{}
	intent.Call.Sender = domain.Address{1}
	intent.Call.Executor = domain.Address{2}
	intent.Call.Data = []byte{3}
	frame := tokenrisk.CallFrame{From: intent.Call.Sender.Hex(), To: intent.Call.Executor.Hex(), Input: "0x03", Error: "execution reverted", Output: "0x219b4c860000000000000000000000000000000000000000000000000000000000000001"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID any `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": frame})
	}))
	defer server.Close()
	n := RPCDispatchNetwork{Read: rpc.NewClient(server.URL, rpc.Options{})}
	if got := n.Diagnose(t.Context(), intent, &Settlement{Hash: domain.Hash{1}}); got != "pool_moved:1" {
		t.Fatal(got)
	}
	// The hot path raises the price bound inside a pool's callback: the
	// executor is still the innermost frame carrying the revert.
	pool := domain.Address{9}.Hex()
	root := frame
	callback := tokenrisk.CallFrame{From: pool, To: intent.Call.Executor.Hex(), Error: "execution reverted", Output: root.Output}
	frame.Calls = []tokenrisk.CallFrame{{From: intent.Call.Executor.Hex(), To: pool, Error: "execution reverted", Output: root.Output, Calls: []tokenrisk.CallFrame{callback}}}
	if got := n.Diagnose(t.Context(), intent, &Settlement{Hash: domain.Hash{1}}); got != "pool_moved:1" {
		t.Fatal("price bound raised in the executor's callback:", got)
	}
	// A contract on the route reverting with the executor's selector is not
	// the executor's price bound.
	frame.Calls = []tokenrisk.CallFrame{{From: intent.Call.Executor.Hex(), To: pool, Error: "execution reverted", Output: root.Output}}
	if got := n.Diagnose(t.Context(), intent, &Settlement{Hash: domain.Hash{1}}); got != "no_transfer_shortfall_evidence" {
		t.Fatal("forged price-bound revert accepted:", got)
	}
	frame = root
	intent.Call.Data = []byte{4}
	if got := n.Diagnose(t.Context(), intent, &Settlement{Hash: domain.Hash{1}}); got != "trace_identity_mismatch" {
		t.Fatal(got)
	}
}

// A route that passed preflight reverts on chain because its token began
// refusing the executor: the first revert records the token. A protected
// start asset refusing the same way is not recorded.
func TestFailedLiveReceiptLearnsRefusingToken(t *testing.T) {
	executor, pool := domain.Address{0xe}, domain.Address{0x03}
	trap, weth := domain.Address{0x22}, domain.Address{0x42}
	transfer := func(token domain.Address) tokenrisk.CallFrame {
		input := "0xa9059cbb" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(executor.Hex()), "0x") + strings.Repeat("0", 62) + "64"
		return tokenrisk.CallFrame{Type: "CALL", From: pool.Hex(), To: token.Hex(), Input: input, Error: "execution reverted"}
	}
	intent := ReactiveIntent{}
	intent.Call.Sender, intent.Call.Executor, intent.Call.Data = domain.Address{1}, executor, []byte{3}
	intent.Route.Steps = []ExecutorStep{{Pool: domain.PoolKey{Address: pool}, TokenOut: trap}}
	frame := tokenrisk.CallFrame{Type: "CALL", From: intent.Call.Sender.Hex(), To: executor.Hex(), Input: "0x03", Error: "execution reverted", Calls: []tokenrisk.CallFrame{
		{Type: "CALL", From: executor.Hex(), To: pool.Hex(), Error: "execution reverted", Calls: []tokenrisk.CallFrame{transfer(weth), transfer(trap)}},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			ID     any    `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var result any = frame
		if req.Method == "eth_call" {
			result = "0x" + strings.Repeat("0", 62) + "ff"
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer server.Close()
	chain := domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 4663}
	risks, err := tokenrisk.Open("", chain)
	if err != nil {
		t.Fatal(err)
	}
	defer risks.Close()
	n := RPCDispatchNetwork{Read: rpc.NewClient(server.URL, rpc.Options{}), Chain: chain, TokenRisks: risks, ProtectedTokens: map[domain.Address]bool{weth: true}}
	settled := &Settlement{Hash: domain.Hash{1}, Block: domain.BlockRef{Number: 9, Hash: domain.Hash{2}, ParentHash: domain.Hash{3}}}
	if err := n.observeFailedTransfer(t.Context(), intent, settled); err != nil {
		t.Fatal(err)
	}
	if settled.TransferDiagnosis != "transfer_refused_recorded" || risks.Check(trap) == nil || risks.Check(weth) != nil {
		t.Fatal(settled.TransferDiagnosis)
	}
}
