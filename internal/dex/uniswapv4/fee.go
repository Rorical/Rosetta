package uniswapv4

import "fmt"

const (
	// feePipsDenominator is 100% in hundredths of a basis point, the V4 fee
	// unit shared with V3.
	feePipsDenominator uint32 = 1_000_000

	// dynamicFeeFlag marks a slot0 lpFee that a hook decides per swap. A
	// hooks-free pool cannot set it.
	dynamicFeeFlag uint32 = 0x800000

	// protocolFeeBits is the width of each packed protocol-fee half in
	// slot0.protocolFee.
	protocolFeeBits = 12
	protocolFeeMask = 1<<protocolFeeBits - 1
)

// SwapFee returns the fee a V4 swap actually charges, in hundredths of a basis
// point: the direction's protocol fee plus the LP fee minus the overlap, since
// the protocol takes its share out of the LP fee rather than adding to it.
//
// protocolFee is the packed slot0 value: the zero-for-one half in the low 12
// bits and the one-for-zero half in the high 12 bits.
func SwapFee(lpFee, protocolFee uint32, zeroForOne bool) (uint32, error) {
	if lpFee&dynamicFeeFlag != 0 {
		return 0, fmt.Errorf("%w: lp fee %#x sets the dynamic-fee flag", ErrInvalidFee, lpFee)
	}
	if lpFee >= feePipsDenominator {
		return 0, fmt.Errorf("%w: lp fee %d pips outside [0, %d)", ErrInvalidFee, lpFee, feePipsDenominator)
	}
	zeroForOnePips := protocolFee & protocolFeeMask
	oneForZeroPips := protocolFee >> protocolFeeBits
	if oneForZeroPips > protocolFeeMask {
		return 0, fmt.Errorf("%w: protocol fee %#x does not pack two 12-bit halves", ErrInvalidFee, protocolFee)
	}

	protocolPips := zeroForOnePips
	if !zeroForOne {
		protocolPips = oneForZeroPips
	}
	overlap := uint32(uint64(protocolPips) * uint64(lpFee) / uint64(feePipsDenominator))
	return protocolPips + lpFee - overlap, nil
}
