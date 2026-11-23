package metricomm

import "errors"

// Reverts of the pool, one error per custom error of the contract.
var (
	// ErrPoolPaused mirrors PoolPaused: swap refuses any non-zero pause level.
	ErrPoolPaused = errors.New("metricomm: pool paused")
	// ErrBidGreaterThanAsk mirrors BidGreaterThanAsk (bid >= ask).
	ErrBidGreaterThanAsk = errors.New("metricomm: bid not below ask")
	// ErrBidIsZero mirrors BidIsZero.
	ErrBidIsZero = errors.New("metricomm: bid is zero")
	// ErrReferencePriceOutOfRange mirrors ReferencePriceOutOfRange: the
	// reference lies outside [bid, ask].
	ErrReferencePriceOutOfRange = errors.New("metricomm: reference price outside bid and ask")
	// ErrBidAskRefAsymmetryOutOfBounds mirrors BidAskRefAsymmetryOutOfBounds.
	ErrBidAskRefAsymmetryOutOfBounds = errors.New("metricomm: bid/ask asymmetry around reference out of bounds")
	// ErrInvalidAmount mirrors InvalidAmount (amountSpecified == 0).
	ErrInvalidAmount = errors.New("metricomm: amount specified is zero")
	// ErrAmountScaledExceedsMax mirrors AmountScaledExceedsMax: the scaled
	// amount does not fit uint128.
	ErrAmountScaledExceedsMax = errors.New("metricomm: scaled amount exceeds uint128")
	// ErrFinalAmountIsZero mirrors FinalAmountIsZero: one of the two native
	// deltas is zero, e.g. the price limit blocks the swap or dust rounds away.
	ErrFinalAmountIsZero = errors.New("metricomm: final amount is zero")
	// ErrBinBalanceOverflow mirrors OverflowedOnFittingBinBalances: the input
	// credited to a bin would exceed uint104.
	ErrBinBalanceOverflow = errors.New("metricomm: bin balance exceeds uint104")
	// ErrArithmeticOverflow covers Solidity Panic(0x11) and SafeCast reverts.
	ErrArithmeticOverflow = errors.New("metricomm: arithmetic overflow")
	// ErrDivisionByZero covers Solidity Panic(0x12).
	ErrDivisionByZero = errors.New("metricomm: division by zero")
)

// ErrInvalidInput reports arguments the contract's ABI could not carry: a nil
// value, an amount outside int128, or a price outside uint128.
var ErrInvalidInput = errors.New("metricomm: invalid input")

// ErrInvalidState reports a State or storage read that cannot describe a pool:
// a bin grid that does not match the immutables, or a current bin outside it.
var ErrInvalidState = errors.New("metricomm: invalid state")
