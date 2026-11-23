package remote

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

var opGasOracle = opPredeploy(0x0f)

var opL1Block = opPredeploy(0x15)

func opPredeploy(last byte) domain.Address {
	address := addressAt(last)
	address[0] = 0x42
	return address
}

type opFeeQuote struct {
	proof    execution.OPFeeEvidence
	l1Number uint64
	l1Price  *big.Int
}

func (s *snapshot) quoteOPFees(ctx context.Context, call execution.ExecutorCall) (opFeeQuote, error) {
	if s.paperFeeState != nil {
		return s.paperFeeState.quoteOPFees(ctx, call)
	}
	raw, err := execution.UnsignedDynamicFeeTx(call, s.chain.ChainID)
	if err != nil {
		return opFeeQuote{}, err
	}
	size, err := execution.OPL1SizeBound(len(raw))
	if err != nil {
		return opFeeQuote{}, err
	}
	code, err := s.CodeHash(ctx, opGasOracle)
	if err != nil || code == (domain.Hash{}) {
		return opFeeQuote{}, fmt.Errorf("OP GasPriceOracle code unavailable")
	}
	data, err := s.opFeeWord(ctx, "getL1FeeUpperBound(uint256)", size)
	if err != nil {
		return opFeeQuote{}, fmt.Errorf("OP Fjord L1 fee bound: %w", err)
	}
	operator, err := s.opFeeWord(ctx, "getOperatorFee(uint256)", call.GasLimit)
	if err != nil {
		return opFeeQuote{}, fmt.Errorf("OP operator fee bound: %w", err)
	}
	l1, err := s.readWord(ctx, opL1Block, "number()")
	if err != nil || !l1.IsUint64() || l1.Sign() == 0 {
		return opFeeQuote{}, fmt.Errorf("OP L1 block context unavailable")
	}
	price, err := s.readWord(ctx, opGasOracle, "l1BaseFee()")
	if err != nil {
		return opFeeQuote{}, err
	}
	return opFeeQuote{proof: execution.OPFeeEvidence{OracleCodeHash: code, L1FeeUpperBound: data, OperatorFeeUpperBound: operator}, l1Number: l1.Uint64(), l1Price: price}, nil
}

func (s *snapshot) opFeeWord(ctx context.Context, signature string, value uint64) (*big.Int, error) {
	if s.paperFeeState != nil {
		return s.paperFeeState.opFeeWord(ctx, signature, value)
	}
	input := make([]byte, 36)
	copy(input, selector(signature))
	new(big.Int).SetUint64(value).FillBytes(input[4:])
	result, err := s.ReadContract(ctx, opGasOracle, input)
	if err != nil {
		return nil, err
	}
	if len(result) != 32 {
		return nil, fmt.Errorf("OP fee oracle returned malformed ABI")
	}
	return new(big.Int).SetBytes(result), nil
}

// EstimateFeeBudget includes all chain-specific costs before minProfit is encoded.
func (s *snapshot) EstimateFeeBudget(ctx context.Context, call execution.ExecutorCall) (*big.Int, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if s.chain.Family == domain.FamilyNitro {
		return (execution.NitroFees{}).Budget(execution.ExecutionEvidence{Call: call})
	}
	quote, err := s.quoteOPFees(ctx, call)
	if err != nil {
		return nil, err
	}
	return (execution.OPFees{}).Budget(execution.ExecutionEvidence{Call: call, OP: &quote.proof})
}
