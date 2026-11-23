package exec

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

// Receipt diagnostics run after canonical settlement, outside quote/sign/send.
// Unsupported tracing preserves paid-fee accounting and records the missing
// diagnosis; only observed balance evidence can add a token exclusion.
func (n RPCDispatchNetwork) observeFailedTransfer(ctx context.Context, intent ReactiveIntent, settled *Settlement) error {
	client := n.Trace
	if client == nil {
		client = n.Read
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var frame tokenrisk.CallFrame
	if err := client.Call(ctx, "debug_traceTransaction", []any{settled.Hash.Hex(), map[string]any{"tracer": "callTracer", "timeout": "2s"}}, &frame); err != nil {
		settled.TransferDiagnosis = "trace_unavailable"
		return nil
	}
	if !strings.EqualFold(frame.From, intent.Call.Sender.Hex()) || !strings.EqualFold(frame.To, intent.Call.Executor.Hex()) || frame.Input != "0x"+hex.EncodeToString(intent.Call.Data) || frame.Error == "" {
		settled.TransferDiagnosis = "trace_identity_mismatch"
		return nil
	}
	settled.TransferDiagnosis = "no_transfer_shortfall_evidence"
	// PoolMoved(uint256) counts only when the executor raised it. Its revert
	// data bubbles up unchanged, so a contract on the route could revert with
	// the same bytes; a lost race restores the route's preflight pass.
	if raw, err := hex.DecodeString(strings.TrimPrefix(frame.Output, "0x")); err == nil && len(raw) == 36 && hex.EncodeToString(raw[:4]) == "219b4c86" && strings.EqualFold(revertOrigin(frame).To, intent.Call.Executor.Hex()) {
		settled.TransferDiagnosis = "pool_moved:" + new(big.Int).SetBytes(raw[4:]).String()
	}
	if n.TokenRisks == nil {
		return nil
	}
	for _, e := range tokenrisk.Detect(frame, settled.Block, intent.Call.Executor) {
		if n.ProtectedTokens[e.Token] {
			continue
		}
		if err := n.TokenRisks.Record(e); err != nil {
			return err
		}
		settled.TransferDiagnosis = "transfer_shortfall_recorded"
	}
	// A token that started refusing the executor after its route passed
	// preflight reverts one send; recording it closes every route through it.
	payers := map[domain.Address]bool{}
	for _, step := range intent.Route.Steps {
		payers[step.Pool.Address] = true
	}
	holds := func(ctx context.Context, token, holder domain.Address, amount *big.Int) (bool, error) {
		var output string
		call := map[string]any{"to": token.Hex(), "data": "0x70a08231" + strings.Repeat("0", 24) + hex.EncodeToString(holder[:])}
		// The parent state: the trace's own pre-state is mid-block.
		if err := client.Call(ctx, "eth_call", []any{call, map[string]any{"blockHash": settled.Block.ParentHash.Hex()}}, &output); err != nil {
			return false, err
		}
		balance, ok := new(big.Int).SetString(strings.TrimPrefix(output, "0x"), 16)
		return ok && balance.Cmp(amount) >= 0, nil
	}
	refused, err := tokenrisk.DetectRefused(ctx, frame, settled.Block, tokenrisk.Refusal{Executor: intent.Call.Executor, Payers: payers, Protected: n.ProtectedTokens, Holds: holds})
	if err != nil {
		settled.TransferDiagnosis = "refusal_balance_unavailable"
		return nil
	}
	for _, e := range refused {
		if err := n.TokenRisks.Record(e); err != nil {
			return err
		}
		settled.TransferDiagnosis = "transfer_refused_recorded"
	}
	return nil
}

// revertOrigin is the innermost failed frame that carries frame's revert
// data: the call that raised it, rather than one that passed it up.
func revertOrigin(frame tokenrisk.CallFrame) tokenrisk.CallFrame {
	for _, call := range frame.Calls {
		if call.Error != "" && strings.EqualFold(call.Output, frame.Output) {
			return revertOrigin(call)
		}
	}
	return frame
}
