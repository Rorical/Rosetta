package remote

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// confirmOP targets op-reth's fee-free debug_traceCall semantics. The only
// additional execution override pre-debits the sender's native fee reserve.
// A Flashblocks snapshot already contains its bound prefix state, obtained
// from the node cache export or verified native prefix replay.
func (s *snapshot) confirmOP(ctx context.Context, request execution.ConfirmationRequest) (*execution.ExecutionEvidence, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if len(request.RawTx) != 0 {
		return nil, fmt.Errorf("OP RPC confirmation requires an unsigned invocation")
	}
	call, err := execution.BuildExecutorCall(ctx, request.Plan, request.Parameters)
	if err != nil {
		return nil, err
	}
	// RPC call mode disables the per-transaction cap. Use the EIP-7825 cap
	// conservatively even when confirming a pre-activation historical block.
	if call.GasLimit > s.block.GasLimit || call.GasLimit > 1<<24 {
		return nil, fmt.Errorf("OP envelope exceeds block or transaction gas limit")
	}
	code, err := s.CodeHash(ctx, call.Executor)
	if err != nil {
		return nil, err
	}
	if code != call.CodeHash {
		return nil, fmt.Errorf("OP executor code identity mismatch")
	}
	code, err = s.CodeHash(ctx, call.Sender)
	if err != nil {
		return nil, err
	}
	if code != (domain.Hash{}) {
		return nil, fmt.Errorf("OP sender must be an EOA")
	}
	nonce, err := s.Nonce(ctx, call.Sender)
	if err != nil || nonce != call.Nonce {
		return nil, fmt.Errorf("OP sender nonce changed")
	}
	price := new(big.Int).Add(s.block.BaseFee, call.TipCap)
	if price.Cmp(call.FeeCap) > 0 {
		return nil, fmt.Errorf("OP fee cap is below base fee and tip")
	}
	quote, err := s.quoteOPFees(ctx, call)
	if err != nil {
		return nil, err
	}
	budget, err := (execution.OPFees{}).Budget(execution.ExecutionEvidence{Call: call, OP: &quote.proof})
	if err != nil {
		return nil, err
	}
	beforeNative, err := s.balance(ctx, call.Sender)
	if err != nil {
		return nil, err
	}
	if beforeNative.Cmp(new(big.Int).Add(budget, call.Value)) < 0 {
		return nil, fmt.Errorf("OP sender cannot fund execution, L1 data, operator fee and value")
	}
	ownerWord, err := s.ReadContract(ctx, call.Executor, selector("owner()"))
	if err != nil || len(ownerWord) != 32 || new(big.Int).SetBytes(ownerWord[:12]).Sign() != 0 {
		return nil, fmt.Errorf("OP executor owner unavailable")
	}
	var owner domain.Address
	copy(owner[:], ownerWord[12:])
	if owner == (domain.Address{}) || opFeeRecipient(owner, s.block.Coinbase) || opFeeRecipient(call.Sender, s.block.Coinbase) {
		return nil, fmt.Errorf("OP confirmation requires owner/sender outside fee recipients")
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

	// Native transaction admission checks feeCap, but prepayment deducts the
	// effective price. Refund unused L2/operator gas after the executor body.
	upfront := new(big.Int).Mul(new(big.Int).SetUint64(call.GasLimit), price)
	upfront.Add(upfront, quote.proof.L1FeeUpperBound).Add(upfront, quote.proof.OperatorFeeUpperBound)
	reservedBalance := new(big.Int).Sub(beforeNative, upfront)
	initial := map[string]any{call.Sender.Hex(): map[string]any{"balance": "0x" + reservedBalance.Text(16)}}
	args := executorArgs(call, s.chain.ChainID)
	var frame struct{ Output, Error, GasUsed string }
	options := map[string]any{"tracer": "callTracer", "tracerConfig": map[string]any{"onlyTopCall": true}, "stateOverrides": initial, "timeout": "1s"}
	if err := s.traceCall(ctx, args, options, &frame); err != nil {
		return nil, fmt.Errorf("OP full executor trace: %w", err)
	}
	gas, err := quantity(frame.GasUsed)
	if err != nil || gas == 0 || gas > call.GasLimit {
		return nil, fmt.Errorf("OP trace returned invalid gas usage")
	}
	if frame.Error != "" && frame.Output == "" {
		frame.Output = "0x" // Out-of-gas and empty reverts may omit return data.
	}
	output, err := hexData(frame.Output)
	if err != nil {
		return nil, fmt.Errorf("OP trace returned malformed output")
	}
	if frame.Error != "" {
		if frame.Output == "0x11157667" && s.risks != nil {
			// Only an amount-invariant failure needs this additional trace. Normal
			// quotes and successful confirmations retain the compact hot path.
			if err := s.observeTransferFailure(ctx, args, options); err != nil {
				return nil, err
			}
		}
		return nil, &executorRevert{Gas: gas, Data: frame.Output, Reason: frame.Error}
	}
	if len(output) != 64 {
		return nil, fmt.Errorf("OP executor returned incomplete ABI")
	}
	profit, loan := new(big.Int).SetBytes(output[:32]), new(big.Int).SetBytes(output[32:])
	if profit.Cmp(request.Plan.MinProfit) < 0 || loan.Cmp(request.Plan.MaxLoanFee) > 0 {
		return nil, fmt.Errorf("OP executor profit/loan guard failed")
	}
	post, err := s.postState(ctx, args, initial)
	if err != nil {
		return nil, err
	}
	account, ok := post[call.Sender.Hex()].(map[string]any)
	if !ok || account["nonce"] != fmt.Sprintf("0x%x", nonce+1) {
		return nil, fmt.Errorf("OP trace did not advance sender nonce")
	}
	rawNative, err := s.assetBalance(ctx, domain.Address{}, call.Sender, post)
	if err != nil {
		return nil, err
	}
	// Profit stays in the executor and wallet principal comes back, so the
	// sender's reserved balance is unchanged by the call itself.
	expected := new(big.Int).Set(reservedBalance)
	// Earlier revm releases reimburse unused execution gas even with charging
	// disabled. Recognize that exact refund, rather than counting it twice or
	// accepting an arbitrary sender balance discrepancy.
	executionRefund := new(big.Int).Mul(new(big.Int).SetUint64(call.GasLimit-gas), price)
	credited := new(big.Int).Sub(rawNative, expected)
	switch {
	case credited.Sign() == 0:
		quote.proof.TraceMode = "fee-free"
	case credited.Cmp(executionRefund) == 0:
		quote.proof.TraceMode = "refund-only"
	default:
		return nil, fmt.Errorf("OP trace changed reserved sender balance; fee-free tracing required")
	}
	operator, err := s.opFeeWord(ctx, "getOperatorFee(uint256)", gas)
	if err != nil {
		return nil, err
	}
	if operator.Cmp(quote.proof.OperatorFeeUpperBound) > 0 {
		return nil, fmt.Errorf("OP operator fee exceeds quoted gas-limit bound")
	}
	quote.proof.OperatorFee = operator
	executionFee := new(big.Int).Mul(new(big.Int).SetUint64(gas), price)
	totalFee := new(big.Int).Add(executionFee, quote.proof.L1FeeUpperBound)
	totalFee.Add(totalFee, operator)
	refund := new(big.Int).Sub(upfront, totalFee)
	refund.Sub(refund, credited)
	settled := new(big.Int).Add(rawNative, refund)
	account["balance"] = "0x" + settled.Text(16)
	afterExecutor, err := s.assetBalance(ctx, request.Plan.Asset, call.Executor, post)
	if err != nil {
		return nil, err
	}
	if afterExecutor.Cmp(new(big.Int).Add(beforeExecutor, profit)) != 0 {
		return nil, fmt.Errorf("OP executor profit balance mismatch")
	}
	afterSender, err := s.assetBalance(ctx, request.Plan.Asset, call.Sender, post)
	if err != nil {
		return nil, err
	}
	expectedSender := new(big.Int).Set(beforeSender)
	if request.Plan.Asset == (domain.Address{}) {
		expectedSender.Sub(expectedSender, totalFee)
	}
	if afterSender.Cmp(expectedSender) != 0 {
		return nil, fmt.Errorf("OP sender principal balance mismatch")
	}
	if owner != call.Sender {
		afterOwner, err := s.assetBalance(ctx, request.Plan.Asset, owner, post)
		if err != nil {
			return nil, err
		}
		if afterOwner.Cmp(beforeOwner) != 0 {
			return nil, fmt.Errorf("OP owner balance changed")
		}
	}
	if err := s.checkCanonical(ctx); err != nil {
		return nil, err
	}

	evidence := &execution.ExecutionEvidence{FeeAccounting: "op-rpc-reserved", OP: &quote.proof, Call: call, Context: execution.ConfirmationContext{Family: domain.FamilyOPStack, ChainID: s.chain.ChainID, Block: s.block.Ref, Timestamp: s.block.Ref.Timestamp, Coinbase: s.block.Coinbase, BlockGasLimit: s.block.GasLimit, BaseFee: new(big.Int).Set(s.block.BaseFee), L1BlockNumber: quote.l1Number, L1UnitPrice: quote.l1Price}, Owner: owner, Profit: profit, LoanFee: loan, GasUsed: gas, PaidGasPrice: price, L1FeeWei: new(big.Int).Set(quote.proof.L1FeeUpperBound), L2FeeWei: executionFee, TotalFeeWei: totalFee, L1FeeEstimated: true, NonceAfter: nonce + 1}
	if err := s.FeeModel().Validate(*evidence); err != nil {
		return nil, err
	}
	return evidence, nil
}

func executorArgs(call execution.ExecutorCall, chainID uint64) map[string]any {
	return map[string]any{"from": call.Sender.Hex(), "to": call.Executor.Hex(), "type": "0x2", "chainId": fmt.Sprintf("0x%x", chainID), "data": "0x" + hex.EncodeToString(call.Data), "value": "0x" + call.Value.Text(16), "nonce": fmt.Sprintf("0x%x", call.Nonce), "gas": fmt.Sprintf("0x%x", call.GasLimit), "maxFeePerGas": "0x" + call.FeeCap.Text(16), "maxPriorityFeePerGas": "0x" + call.TipCap.Text(16)}
}

func opFeeRecipient(address, coinbase domain.Address) bool {
	if address == coinbase {
		return true
	}
	for _, last := range []byte{0x11, 0x19, 0x1a, 0x1b} {
		if address == opPredeploy(last) {
			return true
		}
	}
	return false
}
