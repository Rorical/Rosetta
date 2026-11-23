package remote

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// traceCall applies the native client's complete executor invocation. RPC call
// mode skips nonce/EOA admission, so confirm checks those against the same hash.
// No state or block overrides are supplied to this execution.
func (s *snapshot) confirmNitro(ctx context.Context, request execution.ConfirmationRequest) (*execution.ExecutionEvidence, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if s.chain.Family != domain.FamilyNitro {
		return nil, fmt.Errorf("remote full confirmation currently requires Nitro")
	}
	if len(request.RawTx) != 0 {
		return nil, fmt.Errorf("remote unsigned confirmation does not validate signed envelopes")
	}
	call, err := execution.BuildExecutorCall(ctx, request.Plan, request.Parameters)
	if err != nil {
		return nil, err
	}
	if call.GasLimit > s.block.GasLimit || call.GasLimit >= 1<<53 || call.Nonce >= 1<<53 {
		return nil, fmt.Errorf("remote envelope exceeds block or trace nonce limits")
	}
	version, err := s.readWord(ctx, addressAt(0x64), "arbOSVersion()")
	if err != nil || !version.IsUint64() || version.Uint64() <= 55 {
		return nil, fmt.Errorf("remote ArbOS context unavailable")
	}
	version.Sub(version, big.NewInt(55))
	if err := s.checkComputeLimit(ctx, call.GasLimit, version.Uint64()); err != nil {
		return nil, err
	}
	code, err := s.CodeHash(ctx, call.Executor)
	if err != nil {
		return nil, err
	}
	if code != call.CodeHash {
		return nil, fmt.Errorf("remote executor code identity mismatch")
	}
	code, err = s.CodeHash(ctx, call.Sender)
	if err != nil {
		return nil, err
	}
	if code != (domain.Hash{}) {
		return nil, fmt.Errorf("remote sender must be an EOA")
	}
	nonce, err := s.Nonce(ctx, call.Sender)
	if err != nil || nonce != call.Nonce {
		return nil, fmt.Errorf("remote sender nonce changed")
	}
	price := new(big.Int).Add(s.block.BaseFee, call.TipCap)
	if price.Cmp(call.FeeCap) > 0 {
		return nil, fmt.Errorf("remote fee cap is below base fee and tip")
	}
	beforeNative, err := s.balance(ctx, call.Sender)
	if err != nil {
		return nil, err
	}
	required := new(big.Int).Mul(new(big.Int).SetUint64(call.GasLimit), call.FeeCap)
	required.Add(required, call.Value)
	if beforeNative.Cmp(required) < 0 {
		return nil, fmt.Errorf("remote sender cannot fund gas envelope and value")
	}
	ownerData, err := s.ReadContract(ctx, call.Executor, selector("owner()"))
	if err != nil || len(ownerData) != 32 || new(big.Int).SetBytes(ownerData[:12]).Sign() != 0 {
		return nil, fmt.Errorf("remote executor owner is unavailable")
	}
	var owner domain.Address
	copy(owner[:], ownerData[12:])
	if owner == (domain.Address{}) {
		return nil, fmt.Errorf("remote executor owner is zero")
	}
	beforeOwner, err := s.assetBalance(ctx, request.Plan.Asset, owner, nil)
	if err != nil {
		return nil, err
	}
	beforeSender, err := s.assetBalance(ctx, request.Plan.Asset, call.Sender, nil)
	if err != nil {
		return nil, err
	}
	beforeExecutor, err := s.assetBalance(ctx, request.Plan.Asset, call.Executor, nil)
	if err != nil {
		return nil, err
	}
	args := map[string]any{"from": call.Sender.Hex(), "to": call.Executor.Hex(), "data": "0x" + hex.EncodeToString(call.Data), "gas": fmt.Sprintf("0x%x", call.GasLimit), "nonce": fmt.Sprintf("0x%x", call.Nonce), "maxFeePerGas": "0x" + call.FeeCap.Text(16), "maxPriorityFeePerGas": "0x" + call.TipCap.Text(16), "value": "0x" + call.Value.Text(16)}
	tracer := fmt.Sprintf(`{enter:function(){},exit:function(){},fault:function(){},result:function(ctx,db){return {output:toHex(ctx.output),error:ctx.error||"",gas:ctx.gasUsed,balance:db.getBalance(toAddress("%s")).toString(16),nonce:db.getNonce(toAddress("%s"))};}}`, call.Sender.Hex(), call.Sender.Hex())
	var trace struct {
		Output, Error, Balance string
		Gas, Nonce             uint64
	}
	if err := s.client.Call(ctx, "debug_traceCall", []any{args, s.blockID(), map[string]any{"tracer": tracer, "timeout": "1s"}}, &trace); err != nil {
		return nil, fmt.Errorf("remote native trace (JavaScript tracer required): %w", err)
	}
	output, err := hexData(trace.Output)
	if err != nil || trace.Error != "" || len(output) != 64 || trace.Gas == 0 || trace.Gas > call.GasLimit || trace.Nonce != nonce+1 {
		return nil, fmt.Errorf("remote executor failed or returned incomplete transition evidence: %s", trace.Error)
	}
	profit, loan := new(big.Int).SetBytes(output[:32]), new(big.Int).SetBytes(output[32:])
	if profit.Cmp(request.Plan.MinProfit) < 0 || loan.Cmp(request.Plan.MaxLoanFee) > 0 {
		return nil, fmt.Errorf("remote executor profit/loan guard failed")
	}
	afterNative, err := integer("0x" + trace.Balance)
	if err != nil {
		return nil, err
	}
	// Profit stays in the executor and wallet principal comes back, so the
	// sender's native change is the fee alone.
	fee := new(big.Int).Sub(beforeNative, afterNative)
	if fee.Sign() <= 0 {
		return nil, fmt.Errorf("remote trace did not apply native transaction fees")
	}
	overrides, err := s.postState(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	// Both tracers must describe the same full transaction. Some endpoints
	// expose tracing APIs but omit native fees or nonce changes in diff mode.
	sender, ok := overrides[call.Sender.Hex()].(map[string]any)
	if !ok || sender["nonce"] != fmt.Sprintf("0x%x", trace.Nonce) || sender["balance"] != "0x"+afterNative.Text(16) {
		return nil, fmt.Errorf("remote tracers disagree on sender transition")
	}
	afterExecutor, err := s.assetBalance(ctx, request.Plan.Asset, call.Executor, overrides)
	if err != nil {
		return nil, err
	}
	if afterExecutor.Cmp(new(big.Int).Add(beforeExecutor, profit)) != 0 {
		return nil, fmt.Errorf("remote executor profit balance mismatch")
	}
	afterSender, err := s.assetBalance(ctx, request.Plan.Asset, call.Sender, overrides)
	if err != nil {
		return nil, err
	}
	wantSender := new(big.Int).Set(beforeSender)
	if request.Plan.Asset == (domain.Address{}) {
		wantSender.Sub(wantSender, fee)
	}
	if afterSender.Cmp(wantSender) != 0 {
		return nil, fmt.Errorf("remote sender asset balance mismatch")
	}
	if owner != call.Sender {
		afterOwner, err := s.assetBalance(ctx, request.Plan.Asset, owner, overrides)
		if err != nil {
			return nil, err
		}
		if afterOwner.Cmp(beforeOwner) != 0 {
			return nil, fmt.Errorf("remote owner balance changed")
		}
	}
	l1Price, err := s.readWord(ctx, addressAt(0x6c), "getL1BaseFeeEstimate()")
	if err != nil {
		return nil, err
	}
	current, err := s.client.ExecutionBlock(ctx, fmt.Sprintf("0x%x", s.block.Ref.Number))
	if err != nil || current.Ref.Hash != s.block.Ref.Hash {
		return nil, fmt.Errorf("remote confirmation snapshot is no longer canonical")
	}
	evidence := &execution.ExecutionEvidence{FeeAccounting: "nitro-rpc-total", Call: call, Context: execution.ConfirmationContext{Family: domain.FamilyNitro, ChainID: s.chain.ChainID, Block: s.block.Ref, Timestamp: s.block.Ref.Timestamp, Coinbase: s.block.Coinbase, BlockGasLimit: s.block.GasLimit, BaseFee: new(big.Int).Set(s.block.BaseFee), ArbOSVersion: version.Uint64(), L1BlockNumber: s.block.Ref.L1Block, L1UnitPrice: l1Price}, Owner: owner, Profit: profit, LoanFee: loan, GasUsed: trace.Gas, TotalFeeWei: fee, L1FeeEstimated: true, NonceAfter: trace.Nonce}
	if err := s.FeeModel().Validate(*evidence); err != nil {
		return nil, err
	}
	return evidence, nil
}

func (s *snapshot) checkComputeLimit(ctx context.Context, gas, version uint64) error {
	var maximum *big.Int
	var err error
	if version >= 50 {
		maximum, err = s.readWord(ctx, addressAt(0x6c), "getMaxTxGasLimit()")
	} else {
		var words []byte
		words, err = s.ReadContract(ctx, addressAt(0x6c), selector("getGasAccountingParams()"))
		if err == nil && len(words) == 96 {
			maximum = new(big.Int).SetBytes(words[64:])
		}
	}
	if err != nil || maximum == nil || !maximum.IsUint64() || gas > maximum.Uint64() {
		return fmt.Errorf("remote envelope exceeds or cannot establish native compute limit")
	}
	return nil
}

func addressAt(last byte) domain.Address {
	var address domain.Address
	address[19] = last
	return address
}
func (s *snapshot) readWord(ctx context.Context, to domain.Address, signature string) (*big.Int, error) {
	value, err := s.ReadContract(ctx, to, selector(signature))
	if err != nil {
		return nil, err
	}
	if len(value) != 32 {
		return nil, fmt.Errorf("remote %s returned malformed ABI", signature)
	}
	return new(big.Int).SetBytes(value), nil
}

func (s *snapshot) assetBalance(ctx context.Context, asset, owner domain.Address, overrides map[string]any) (*big.Int, error) {
	if asset == (domain.Address{}) {
		if account, ok := overrides[owner.Hex()].(map[string]any); ok {
			if value, ok := account["balance"].(string); ok {
				return integer(value)
			}
		}
		return s.balance(ctx, owner)
	}
	data := make([]byte, 36)
	copy(data, selector("balanceOf(address)"))
	copy(data[16:], owner[:])
	word, err := s.contractCall(ctx, asset, data, overrides)
	if err != nil || len(word) != 32 {
		return nil, fmt.Errorf("remote balanceOf returned malformed ABI")
	}
	return new(big.Int).SetBytes(word), nil
}
