package exec

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ConfirmationContext describes the EVM execution position. Block is the
// execution header, which is distinct from a tentative branch's base anchor.
type ConfirmationContext struct {
	Family        domain.ChainFamily `json:",omitempty"`
	ChainID       uint64
	Block         domain.BlockRef
	Timestamp     uint64
	Coinbase      domain.Address
	BlockGasLimit uint64
	BaseFee       *big.Int
	ArbOSVersion  uint64
	L1BlockNumber uint64
	L1UnitPrice   *big.Int
}

// ExecutionEvidence is produced by a full executor call. Estimated L1 cost is
// explicit; budget-based profitability must hold before an unsigned call is ready.
type ExecutionEvidence struct {
	// FeeAccounting is empty for native split-fee evidence. RPC tracing may
	// prove the total sender charge without exposing the ArbOS poster split.
	FeeAccounting  string         `json:",omitempty"`
	OP             *OPFeeEvidence `json:",omitempty"`
	Call           ExecutorCall
	Context        ConfirmationContext
	Transaction    domain.Hash
	Owner          domain.Address
	Profit         *big.Int
	LoanFee        *big.Int
	GasUsed        uint64
	GasUsedForL1   uint64
	PaidGasPrice   *big.Int
	TotalFeeWei    *big.Int
	L1FeeWei       *big.Int
	L2FeeWei       *big.Int
	L1FeeEstimated bool
	NonceAfter     uint64
}

// OracleRound records a snapshot-bound AggregatorV3 observation. Price feeds
// must share the same quote currency; a sequencer feed instead uses status 0/1.
type OracleRound struct {
	Feed            domain.Address
	CodeHash        domain.Hash
	Decimals        uint8
	RoundID         *big.Int
	Answer          *big.Int
	StartedAt       uint64
	UpdatedAt       uint64
	AnsweredInRound *big.Int
}

// FeeConversion converts native wei to atomic profit-asset units. Observations
// are retained with the plan so rounding, freshness and denomination are reviewable.
type FeeConversion struct {
	Numerator     *big.Int
	Denominator   *big.Int
	AssetDecimals uint8
	NativePrice   *OracleRound
	AssetPrice    *OracleRound
	Sequencer     *OracleRound
	MaxPriceAge   uint64
	GraceSeconds  uint64
}
