package arb

import (
	"math"
	"math/big"

	poolmath "github.com/Rorical/Rosetta/internal/math"
)

// Bounds enclose the existing shallow CP surrogate, including when that model
// approximates another protocol. They are not proofs about actual pool output.
type modelInterval struct {
	rateLow, rateHigh, impactLow float64
	valid                        bool
}

// roundDown equals max(0, math.Nextafter(value, -Inf)) and roundUp equals
// math.Nextafter(value, +Inf) for every input, NaN and infinities included.
// Both run once per leg and rotation while ranking; unlike Nextafter they
// inline, and stepping the IEEE bit pattern is exactly Nextafter's rule.
func roundDown(value float64) float64 {
	if value > 0 {
		return math.Float64frombits(math.Float64bits(value) - 1)
	}
	if value != value {
		return math.NaN()
	}
	return 0
}

func roundUp(value float64) float64 {
	switch {
	case value != value:
		return math.NaN()
	case value > math.MaxFloat64:
		return value
	case value > 0:
		return math.Float64frombits(math.Float64bits(value) + 1)
	case value < 0:
		return math.Float64frombits(math.Float64bits(value) - 1)
	}
	return math.Float64frombits(1)
}

func integerInterval(value *big.Int) (float64, float64) {
	number, accuracy := value.Float64()
	low, high := number, number
	if accuracy == big.Above {
		low = roundDown(number)
	}
	if accuracy == big.Below {
		high = roundUp(number)
	}
	return low, high
}

func ratioInterval(numerator, denominator *big.Int) (float64, float64) {
	value, exact := new(big.Rat).SetFrac(numerator, denominator).Float64()
	if exact {
		return value, value
	}
	return roundDown(value), roundUp(value)
}

func newModelInterval(leg poolmath.ClosedFormLeg) modelInterval {
	if leg.InReserve == nil || leg.OutReserve == nil || leg.InReserve.Sign() <= 0 || leg.OutReserve.Sign() <= 0 || leg.FeePips >= 1000000 {
		return modelInterval{}
	}
	fee := big.NewInt(int64(1000000 - leg.FeePips))
	denominator := new(big.Int).Mul(leg.InReserve, big.NewInt(1000000))
	numerator := new(big.Int).Mul(leg.OutReserve, fee)
	low, high := ratioInterval(numerator, denominator)
	impact, _ := ratioInterval(fee, denominator)
	return modelInterval{rateLow: low, rateHigh: high, impactLow: impact, valid: true}
}

// F(x)=R*x/(1+q*x). Compose R upwards and q downwards. For R>1,
// max(F(x)-x) <= (R-1)^2/(4*q), because (sqrt(R)+1)^2 >= 4.
// A linear bound over the funding maximum supplies a second safe ceiling.
func modelCycleUpper(legs []modelInterval, offset int, minimumLow, maximumHigh float64) float64 {
	rateLow, rateHigh, impactLow := 1.0, 1.0, 0.0
	for i := range legs {
		leg := legs[(i+offset)%len(legs)]
		if !leg.valid {
			return math.Inf(1)
		}
		impactLow = roundDown(impactLow + roundDown(leg.impactLow*rateLow))
		rateLow = roundDown(rateLow * leg.rateLow)
		rateHigh = roundUp(rateHigh * leg.rateHigh)
	}
	if math.IsNaN(rateHigh) || math.IsNaN(impactLow) || math.IsInf(rateHigh, 1) {
		return math.Inf(1)
	}
	if rateHigh <= 1 {
		return 0
	}
	denominator := roundDown(1 + roundDown(impactLow*minimumLow))
	if rateHigh <= denominator {
		return 0
	}
	difference := roundUp(rateHigh - 1)
	upper := roundUp(difference * maximumHigh)
	if impactLow > 0 {
		peak := roundUp(roundUp(difference*difference) / roundDown(4*impactLow))
		if !math.IsNaN(peak) {
			upper = min(upper, peak)
		}
	}
	if math.IsNaN(upper) {
		return math.Inf(1)
	}
	return upper
}

// ceilFloat copies a finite, nonnegative float ceiling without allocating a
// big.Float or converting through a rounded decimal representation.
func ceilFloat(destination *big.Int, value float64) {
	if value <= 0 {
		destination.SetInt64(0)
		return
	}
	bits := math.Float64bits(value)
	exponent := int((bits>>52)&0x7ff) - 1023 - 52
	mantissa := (bits & ((uint64(1) << 52) - 1)) | (uint64(1) << 52)
	if exponent >= 0 {
		destination.SetUint64(mantissa)
		destination.Lsh(destination, uint(exponent))
		return
	}
	shift := uint(-exponent)
	if shift > 52 {
		destination.SetInt64(1)
		return
	}
	whole := mantissa >> shift
	if mantissa&((uint64(1)<<shift)-1) != 0 {
		whole++
	}
	destination.SetUint64(whole)
}
