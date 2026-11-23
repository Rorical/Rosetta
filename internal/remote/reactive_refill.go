package remote

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// refillEvery spaces refill attempts: a sweep must land before the unwrap
// that follows it can see the owner's wrapped balance.
const refillEvery = 30 * time.Second

// refillStep is the next maintenance toward native_refill's target, or nil.
// The wallet's wrapped balance is unwrapped first; profit is swept from the
// executor only for what the wallet lacks, so a refill takes at most a sweep
// and an unwrap.
func refillStep(native, below, target, walletWrapped, executorWrapped *big.Int) (kind string, amount *big.Int) {
	if native.Cmp(below) >= 0 {
		return "", nil
	}
	need := new(big.Int).Sub(target, native)
	if walletWrapped.Cmp(need) >= 0 {
		return execution.MaintenanceUnwrap, need
	}
	if short := new(big.Int).Sub(need, walletWrapped); executorWrapped.Sign() > 0 {
		if executorWrapped.Cmp(short) < 0 {
			short = executorWrapped
		}
		return execution.MaintenanceSweep, short
	}
	if walletWrapped.Sign() > 0 {
		return execution.MaintenanceUnwrap, new(big.Int).Set(walletWrapped)
	}
	return "", nil
}

// refill runs on the refresh goroutine after each context refresh, failed or
// not: a low balance is exactly when the refresh fails. It reads balances at
// latest and sends at most one maintenance transaction, when the lane is free.
func (s *reactiveRuntime) refill(ctx context.Context, now time.Time) {
	if s.dispatcher == nil || s.settings.refillBelow == nil || now.Before(s.refillAfter) || s.gate.Paused() || s.dispatcher.Busy() {
		return
	}
	s.refillAfter = now.Add(refillEvery)
	record, err := s.refillOnce(ctx, now)
	if record == nil && err == nil {
		return
	}
	if record == nil {
		record = map[string]any{}
	}
	record["type"], record["at_ms"] = "maintenance", now.UnixMilli()
	if err != nil {
		record["error"] = err.Error()
		slog.Warn("native refill failed", "error", err)
	}
	s.note(record)
}

func (s *reactiveRuntime) refillOnce(ctx context.Context, now time.Time) (map[string]any, error) {
	cfg, wrapped := s.settings.planning, s.settings.wrapped
	var nativeHex string
	if err := s.client.Call(ctx, "eth_getBalance", []any{cfg.Sender.Hex(), "latest"}, &nativeHex); err != nil {
		return nil, fmt.Errorf("sender balance: %w", err)
	}
	native, ok := new(big.Int).SetString(strings.TrimPrefix(nativeHex, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("sender balance: malformed %q", nativeHex)
	}
	if native.Cmp(s.settings.refillBelow) >= 0 {
		return nil, nil
	}
	walletWrapped, err := s.refillBalance(ctx, wrapped, cfg.Sender)
	if err != nil {
		return nil, err
	}
	executorWrapped, err := s.refillBalance(ctx, wrapped, cfg.Executor)
	if err != nil {
		return nil, err
	}
	kind, amount := refillStep(native, s.settings.refillBelow, s.settings.refillTarget, walletWrapped, executorWrapped)
	record := map[string]any{"native": native, "wallet_wrapped": walletWrapped, "executor_wrapped": executorWrapped}
	if kind == "" {
		record["status"] = "nothing_to_refill"
		return record, nil
	}
	record["kind"], record["amount"] = kind, amount
	var block struct {
		BaseFee string `json:"baseFeePerGas"`
	}
	if err := s.client.Call(ctx, "eth_getBlockByNumber", []any{"latest", false}, &block); err != nil {
		return record, fmt.Errorf("base fee: %w", err)
	}
	base, ok := new(big.Int).SetString(strings.TrimPrefix(block.BaseFee, "0x"), 16)
	if !ok {
		return record, fmt.Errorf("base fee: malformed %q", block.BaseFee)
	}
	// Estimate the exact call; Nitro's estimate includes its L1 poster gas.
	to, data := execution.Maintenance{Kind: kind, Token: wrapped, Amount: amount}.Call(cfg.Executor)
	var gasHex string
	if err := s.client.Call(ctx, "eth_estimateGas", []any{map[string]any{"from": cfg.Sender.Hex(), "to": to.Hex(), "data": "0x" + hex.EncodeToString(data)}, "latest"}, &gasHex); err != nil {
		return record, fmt.Errorf("%s estimate: %w", kind, err)
	}
	gas, ok := new(big.Int).SetString(strings.TrimPrefix(gasHex, "0x"), 16)
	if !ok || !gas.IsUint64() {
		return record, fmt.Errorf("%s estimate: malformed %q", kind, gasHex)
	}
	m := execution.NewMaintenance(kind, wrapped, amount, gas.Uint64()*3/2, new(big.Int).Mul(base, big.NewInt(2)), new(big.Int), now)
	current := func() bool { return s.failure.Load() == nil && !s.gate.Paused() }
	hash, err := s.dispatcher.Maintain(ctx, m, current)
	if errors.Is(err, execution.ErrDispatchBusy) {
		// An arbitrage took the lane first; the next check retries.
		s.refillAfter = time.Time{}
		return nil, nil
	}
	record["transaction"], record["budget"] = hash, m.Budget
	if err != nil {
		return record, err
	}
	record["status"] = "sent"
	return record, nil
}

func (s *reactiveRuntime) refillBalance(ctx context.Context, token, holder domain.Address) (*big.Int, error) {
	var output string
	call := map[string]any{"to": token.Hex(), "data": "0x70a08231" + strings.Repeat("0", 24) + hex.EncodeToString(holder[:])}
	if err := s.client.Call(ctx, "eth_call", []any{call, "latest"}, &output); err != nil {
		return nil, fmt.Errorf("wrapped balance of %s: %w", holder.Hex(), err)
	}
	balance, ok := new(big.Int).SetString(strings.TrimPrefix(output, "0x"), 16)
	if !ok {
		return nil, fmt.Errorf("wrapped balance of %s: malformed %q", holder.Hex(), output)
	}
	return balance, nil
}
