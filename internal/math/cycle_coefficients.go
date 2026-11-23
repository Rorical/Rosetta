package math

import (
	"fmt"
	"math/big"
)

// CycleCoefficients owns the exact shallow curve F(x)=A*x/(B+C*x).
// Set copies its input. A cyclic rotation changes only C.
// Like big.Int, values must not be copied by assignment after first use.
type CycleCoefficients struct{ a, b, c big.Int }

func (m *CycleCoefficients) Set(other *CycleCoefficients) {
	m.a.Set(&other.a)
	m.b.Set(&other.b)
	m.c.Set(&other.c)
}

func (m *CycleCoefficients) SetLeg(leg ClosedFormLeg) error {
	if err := validateClosedFormLeg("cycle leg", leg); err != nil {
		return err
	}
	m.c.SetUint64(uint64(feePipsDenominator - leg.FeePips))
	m.a.Mul(leg.OutReserve, &m.c)
	m.b.Mul(leg.InReserve, feePipsDenominatorBig)
	return nil
}

// CycleEstimator reuses sqrt(A*B) only after comparing both exact products.
// Prefix replacement and arbitrary funding rotations may change C without
// invalidating the root. No assumptions about path identity or caller order
// are needed, and no division of large composition coefficients is required.
type CycleEstimator struct {
	Curve       CycleCoefficients
	a, b, root  big.Int
	left, right big.Int
}

// Append applies a prepared leg or segment after Curve. Scratch integers stay
// with this estimator, avoiding a pool round trip for every route hop.
// Both curves must be initialized from valid legs.
func (e *CycleEstimator) Append(suffix *CycleCoefficients) {
	m := &e.Curve
	e.left.Mul(&suffix.b, &m.c)
	e.right.Mul(&suffix.c, &m.a)
	m.c.Add(&e.left, &e.right)
	e.left.Mul(&m.a, &suffix.a)
	e.right.Mul(&m.b, &suffix.b)
	m.a, e.left = e.left, m.a
	m.b, e.right = e.right, m.b
}

func (e *CycleEstimator) EstimateInto(input, net, minimum, maximum *big.Int) error {
	m := &e.Curve
	if minimum == nil || maximum == nil || minimum.Sign() <= 0 || minimum.Cmp(maximum) > 0 || m.a.Sign() <= 0 || m.b.Sign() <= 0 || m.c.Sign() <= 0 {
		return fmt.Errorf("%w: cycle coefficients or input bounds", ErrInvalidArgument)
	}
	if e.a.Cmp(&m.a) != 0 || e.b.Cmp(&m.b) != 0 {
		e.root.SetInt64(0)
		e.a.Set(&m.a)
		e.b.Set(&m.b)
	}
	w := cycleWorkspaces.Get().(*cycleWorkspace)
	defer w.release()
	return w.estimate(input, net, &m.a, &m.b, &m.c, minimum, maximum, &e.root)
}
