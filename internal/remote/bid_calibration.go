package remote

import (
	"math"
	"time"
)

const (
	calibrationPoints = 256
	// calibrationStrength is a ridge penalty on the intercept, worth roughly
	// that many outcomes at p = 0.5: ten sends move it little, a hundred
	// decide. calibrationSlopeStrength penalizes the slope per e-fold of
	// worth: on a typical 70-send sample it keeps about 84% of the
	// unpenalized slope, and two outcomes at opposite ends of a 40-fold worth
	// range move the shift by under one logit.
	calibrationStrength      = 4.0
	calibrationSlopeStrength = 2.0
	calibrationMaxShift      = 4.0
	calibrationMaxSlope      = 2.0
)

// calibration corrects the model's win probability with our own outcomes,
// by opportunity size, local decision age and fast/full lane. The worth
// component retains the original interpretation below; age and lane are
// additional jointly fitted ridge terms, without a causal timing claim:
//
//	P = σ(logit(raw) + δ0 + δ1·x),   x = ln(worth) − c
//
// worth is the surplus per expected gas the decision priced and c the
// outcomes' decayed mean log worth, so δ0 is the correction at a typical
// opportunity and δ1 its change per e-fold of worth. Small sends win far more
// often than large ones at the same predicted probability: rivals contest
// large opportunities far more often than their presence over all windows
// says, so one intercept is too low for the small and too high for the large.
//
// The jointly fitted terms maximize the decayed likelihood of recent outcomes under a
// ridge penalty that keeps them near zero until outcomes accumulate. An
// outcome without a recorded worth (format 2) informs δ0 alone. Worth outside
// the outcomes' range is clamped to it, so the slope never extrapolates.
type calibration struct {
	points                     []calibrationPoint // a ring
	next                       int
	intercept, slope           float64
	center                     float64 // c, ln wei per gas
	ageSlope, laneSlope        float64
	ageCenter, ageLow, ageHigh float64
	low, high                  float64 // range of x over outcomes with a worth
}

type calibrationPoint struct {
	raw   float64
	ageNS int64
	lane  string
	worth uint64 // 0 if not recorded
	won   bool
	at    time.Time
}

func (c *calibration) add(point calibrationPoint) {
	if len(c.points) < calibrationPoints {
		c.points = append(c.points, point)
	} else {
		c.points[c.next] = point
		c.next = (c.next + 1) % calibrationPoints
	}
	c.fit()
}

// shift is the logit correction for an opportunity of this worth.
func (c *calibration) shift(worth uint64) float64 {
	return c.shiftAt(worth, 0, "")
}

// centerWorth is c in wei per gas, 0 before any outcome with a worth.
func (c *calibration) centerWorth() uint64 {
	if c.center == 0 {
		return 0
	}
	return uint64(math.Round(math.Exp(c.center)))
}

// fit maximizes the penalized decayed log-likelihood by Newton's method with
// step halving; the objective is strictly concave, so the maximum is unique
// and the result depends only on the outcomes. Weights halve every
// bidHalfLife before the newest outcome.
func (c *calibration) fit() {
	c.intercept, c.slope, c.center, c.low, c.high = 0, 0, 0, 0, 0
	c.ageSlope, c.laneSlope, c.ageCenter, c.ageLow, c.ageHigh = 0, 0, 0, 0, 0
	if len(c.points) == 0 {
		return
	}
	var newest time.Time
	for _, p := range c.points {
		if p.at.After(newest) {
			newest = p.at
		}
	}
	var mass, sum, ageMass, ageSum float64
	for _, p := range c.points {
		weight := decay(newest.Sub(p.at), bidHalfLife)
		if p.worth > 0 {
			mass += weight
			sum += weight * math.Log(float64(p.worth))
		}
		if p.ageNS > 0 {
			ageMass += weight
			ageSum += weight * ageFeature(p.ageNS)
		}
	}
	if mass > 0 {
		c.center = sum / mass
	}
	if ageMass > 0 {
		c.ageCenter = ageSum / ageMass
	}
	terms := make([]calibrationTerm, len(c.points))
	for i, p := range c.points {
		raw := min(max(p.raw, 1e-3), 1-1e-3)
		t := calibrationTerm{weight: decay(newest.Sub(p.at), bidHalfLife), logit: math.Log(raw / (1 - raw)), x: [4]float64{1, 0, 0, 0}}
		if p.worth > 0 {
			t.x[1] = math.Log(float64(p.worth)) - c.center
			c.low, c.high = min(c.low, t.x[1]), max(c.high, t.x[1])
		}
		if p.ageNS > 0 {
			t.x[2] = ageFeature(p.ageNS) - c.ageCenter
			c.ageLow, c.ageHigh = min(c.ageLow, t.x[2]), max(c.ageHigh, t.x[2])
		}
		if p.lane == "fast" {
			t.x[3] = 1
		}
		if p.won {
			t.y = 1
		}
		terms[i] = t
	}
	coefficients := fitCalibration(terms)
	c.intercept = min(max(coefficients[0], -calibrationMaxShift), calibrationMaxShift)
	c.slope = min(max(coefficients[1], -calibrationMaxSlope), calibrationMaxSlope)
	c.ageSlope = min(max(coefficients[2], -calibrationMaxSlope), calibrationMaxSlope)
	c.laneSlope = min(max(coefficients[3], -calibrationMaxShift), calibrationMaxShift)
}

// Age is local feed receipt to decision, not unobserved sequencer arrival.
// Missing legacy features contribute no adjustment. Clamp observed ranges to
// avoid extrapolating from a few old sends to a new latency regime.
func ageFeature(ns int64) float64 { return math.Log1p(float64(ns) / float64(100*time.Millisecond)) }
func (c *calibration) shiftAt(worth uint64, ageNS int64, lane string) float64 {
	x := min(max(math.Log(float64(max(worth, 1)))-c.center, c.low), c.high)
	value := c.intercept + c.slope*x
	if ageNS > 0 {
		value += c.ageSlope * min(max(ageFeature(ageNS)-c.ageCenter, c.ageLow), c.ageHigh)
	}
	if lane == "fast" {
		value += c.laneSlope
	}
	return min(max(value, -calibrationMaxShift), calibrationMaxShift)
}

// logSigmoid is ln σ(z) without overflow.
func logSigmoid(z float64) float64 {
	if z >= 0 {
		return -math.Log1p(math.Exp(-z))
	}
	return z - math.Log1p(math.Exp(z))
}

// ordered returns the outcomes oldest first.
func (c *calibration) ordered() []calibrationPoint { return ringOrder(c.points, c.next) }
