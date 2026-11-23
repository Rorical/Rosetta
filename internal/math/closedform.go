package math

import (
	"fmt"
	"math/big"
)

// ClosedFormTwoLeg returns the integer input that maximises net output of an
// A->B->A cycle under constant liquidity, together with the gross output and
// net profit at that input.
//
// With input reserves X and output reserves Y per leg and c = 1 - fee, the
// continuous gross output is G(d) = K*d/(M+N*d) with
//
//	K = c1*c2*Y1*Y2, M = X1*X2, N = c1*(X2 + c2*Y1)
//
// and the net-profit maximiser is d* = (sqrt(K*M)-M)/N when K > M. The function
// computes d* with exact integer sqrt, evaluates floor(d*) and floor(d*)+1 with
// the gross floored from the exact rational, and returns the candidate with the
// larger net. Ties prefer the smaller input. When no integer input yields
// positive net, it returns zero values and profitable=false.
func ClosedFormTwoLeg(leg1, leg2 ClosedFormLeg) (dStar, gross, net *big.Int, profitable bool, err error) {
	if err := validateClosedFormLeg("leg1", leg1); err != nil {
		return nil, nil, nil, false, err
	}
	if err := validateClosedFormLeg("leg2", leg2); err != nil {
		return nil, nil, nil, false, err
	}

	c1 := big.NewInt(int64(feePipsDenominator - leg1.FeePips))
	c2 := big.NewInt(int64(feePipsDenominator - leg2.FeePips))

	// Scale by 1e12 so K = kn/scale and N = nn/scale are integer fractions.
	scale := big.NewInt(feePipsDenominator * feePipsDenominator)

	kn := new(big.Int).Mul(c1, c2)
	kn.Mul(kn, leg1.OutReserve)
	kn.Mul(kn, leg2.OutReserve)

	mn := new(big.Int).Mul(leg1.InReserve, leg2.InReserve)

	nn := new(big.Int).Mul(big.NewInt(feePipsDenominator), leg2.InReserve)
	nn.Add(nn, new(big.Int).Mul(c2, leg1.OutReserve))
	nn.Mul(nn, c1)

	// d* = (sqrt(K*M)-M)/N = (sqrt(kn*mn*scale)-mn*scale)/nn.
	mnScaled := new(big.Int).Mul(mn, scale)
	if kn.Cmp(mnScaled) <= 0 {
		return new(big.Int), new(big.Int), new(big.Int), false, nil
	}
	root := new(big.Int).Sqrt(new(big.Int).Mul(kn, mnScaled))
	base := new(big.Int).Div(new(big.Int).Sub(root, mnScaled), nn)

	grossAt := func(d *big.Int) (*big.Int, *big.Int) {
		if d.Sign() <= 0 {
			return new(big.Int), new(big.Int)
		}
		denominator := new(big.Int).Add(mnScaled, new(big.Int).Mul(nn, d))
		value := new(big.Int).Div(new(big.Int).Mul(kn, d), denominator)
		return value, new(big.Int).Sub(value, d)
	}

	const candidates = 2
	bestD := new(big.Int)
	bestGross := new(big.Int)
	bestNet := new(big.Int)
	for i := int64(0); i < candidates; i++ {
		d := new(big.Int).Add(base, big.NewInt(i))
		value, netAtD := grossAt(d)
		if netAtD.Cmp(bestNet) > 0 {
			bestD.Set(d)
			bestGross.Set(value)
			bestNet.Set(netAtD)
		}
	}

	if bestNet.Sign() <= 0 {
		return new(big.Int), new(big.Int), new(big.Int), false, nil
	}
	return bestD, bestGross, bestNet, true, nil
}

func validateClosedFormLeg(name string, leg ClosedFormLeg) error {
	if err := checkPositive(name+" in reserve", leg.InReserve); err != nil {
		return err
	}
	if err := checkPositive(name+" out reserve", leg.OutReserve); err != nil {
		return err
	}
	if leg.FeePips >= feePipsDenominator {
		return fmt.Errorf("%w: %s fee %d pips", ErrInvalidArgument, name, leg.FeePips)
	}
	return nil
}
