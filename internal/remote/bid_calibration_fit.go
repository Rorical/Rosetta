package remote

import "math"

type calibrationTerm struct {
	weight, logit, y float64
	x                [4]float64
}

// Four-feature ridge logistic calibration: intercept, worth, local age, lane.
// Fixed-size Newton steps run when outcomes resolve, never per tip candidate.
func fitCalibration(terms []calibrationTerm) [4]float64 {
	penalty := [4]float64{calibrationStrength, calibrationSlopeStrength, calibrationSlopeStrength, calibrationStrength}
	dot := func(a, b [4]float64) float64 {
		var sum float64
		for i := range a {
			sum += a[i] * b[i]
		}
		return sum
	}
	objective := func(a [4]float64) float64 {
		var f float64
		for i := range a {
			f -= penalty[i] * a[i] * a[i] / 2
		}
		for _, t := range terms {
			z := t.logit + dot(a, t.x)
			f += t.weight * (t.y*logSigmoid(z) + (1-t.y)*logSigmoid(-z))
		}
		return f
	}
	var a [4]float64
	for range 50 {
		var system [4][5]float64
		for i := range a {
			system[i][i] = penalty[i]
			system[i][4] = -penalty[i] * a[i]
		}
		for _, t := range terms {
			p := 1 / (1 + math.Exp(-(t.logit + dot(a, t.x))))
			r, v := t.weight*(t.y-p), t.weight*p*(1-p)
			for i := range a {
				system[i][4] += r * t.x[i]
				for j := range a {
					system[i][j] += v * t.x[i] * t.x[j]
				}
			}
		}
		// Ridge makes the Hessian positive definite; Gaussian elimination has
		// nonzero pivots even with constant or missing features.
		for i := range a {
			pivot := system[i][i]
			for k := i; k < 5; k++ {
				system[i][k] /= pivot
			}
			for j := range a {
				if j == i {
					continue
				}
				factor := system[j][i]
				for k := i; k < 5; k++ {
					system[j][k] -= factor * system[i][k]
				}
			}
		}
		var norm float64
		for i := range a {
			norm += math.Abs(system[i][4])
		}
		if norm < 1e-9 {
			break
		}
		current, step := objective(a), 1.0
		accepted := false
		for range 30 {
			candidate := a
			for i := range a {
				candidate[i] += step * system[i][4]
			}
			if objective(candidate) >= current {
				a, accepted = candidate, true
				break
			}
			step /= 2
		}
		if !accepted {
			break
		}
	}
	return a
}
