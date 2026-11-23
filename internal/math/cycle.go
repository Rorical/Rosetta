package math

import (
	"fmt"
	"math/big"
	"sync"
)

// CycleCanProfit tests the exact zero-input slope of the shallow model. A
// slope <=1 rules out positive profit at every positive input. This bound is
// independent of the cycle's funding rotation; it uses no floating point.
func CycleCanProfit(legs []ClosedFormLeg) (bool, error) {
	if len(legs) < 2 {
		return false, fmt.Errorf("%w: cycle needs at least two legs", ErrInvalidArgument)
	}
	work := cycleWorkspaces.Get().(*cycleWorkspace)
	defer work.release()
	work.a.SetInt64(1)
	work.b.SetInt64(1)
	for i, leg := range legs {
		if leg.InReserve == nil || leg.OutReserve == nil || leg.InReserve.Sign() <= 0 || leg.OutReserve.Sign() <= 0 || leg.FeePips >= feePipsDenominator {
			return false, validateClosedFormLeg(fmt.Sprintf("leg %d", i), leg)
		}
		work.fee.SetUint64(uint64(feePipsDenominator - leg.FeePips))
		work.legA.Mul(leg.OutReserve, &work.fee)
		work.legB.Mul(leg.InReserve, feePipsDenominatorBig)
		work.left.Mul(&work.a, &work.legA)
		work.right.Mul(&work.b, &work.legB)
		work.a, work.left = work.left, work.a
		work.b, work.right = work.right, work.b
	}
	return work.a.Cmp(&work.b) > 0, nil
}

// EstimateCycle composes constant-product legs into F(x)=A*x/(B+C*x) and
// returns a bounded input hint and its estimated profit. V3 virtual reserves
// make this a local liquidity approximation; callers must quote actual ticks.
// Intermediate swap rounding is omitted, so this is never an execution result.
func EstimateCycle(legs []ClosedFormLeg, minimum, maximum *big.Int) (input, net *big.Int, err error) {
	input, net = new(big.Int), new(big.Int)
	if err := EstimateCycleInto(input, net, legs, minimum, maximum); err != nil {
		return nil, nil, err
	}
	return input, net, nil
}

// EstimateCycleInto writes the same estimate into caller-owned integers. The
// destinations must be distinct and must not alias any reserve or input bound.
func EstimateCycleInto(input, net *big.Int, legs []ClosedFormLeg, minimum, maximum *big.Int) error {
	if len(legs) < 2 || minimum == nil || maximum == nil || minimum.Sign() <= 0 || minimum.Cmp(maximum) > 0 {
		return fmt.Errorf("%w: cycle legs or input bounds", ErrInvalidArgument)
	}
	work := cycleWorkspaces.Get().(*cycleWorkspace)
	defer work.release()
	a, b, c := &work.a, &work.b, &work.c
	a.SetInt64(1)
	b.SetInt64(1)
	c.SetInt64(0)
	for i, leg := range legs {
		// Build diagnostic strings only on invalid external inputs.
		if leg.InReserve == nil || leg.OutReserve == nil || leg.InReserve.Sign() <= 0 || leg.OutReserve.Sign() <= 0 || leg.FeePips >= feePipsDenominator {
			err := validateClosedFormLeg(fmt.Sprintf("leg %d", i), leg)
			return err
		}
		work.fee.SetUint64(uint64(feePipsDenominator - leg.FeePips))
		work.legA.Mul(leg.OutReserve, &work.fee)
		work.legB.Mul(leg.InReserve, feePipsDenominatorBig)
		work.left.Mul(&work.legB, c)
		work.right.Mul(&work.fee, a)
		c.Add(&work.left, &work.right)
		work.left.Mul(a, &work.legA)
		work.right.Mul(b, &work.legB)
		work.a, work.left = work.left, work.a
		work.b, work.right = work.right, work.b
	}
	return work.estimate(input, net, a, b, c, minimum, maximum, nil)
}

func (work *cycleWorkspace) estimate(input, net, a, b, c, minimum, maximum, root *big.Int) error {
	// A/B is the zero-input slope. When it is <=1, profit is nonincreasing
	// for positive inputs, so no square root or endpoint search is needed.
	if a.Cmp(b) <= 0 {
		work.profitAt(net, minimum, a, b, c)
		input.Set(minimum)
		return nil
	}
	// F(x)/x decreases: if even the smallest funded amount cannot break
	// even, the profit derivative is already negative throughout the range.
	work.left.Mul(c, minimum)
	work.left.Add(&work.left, b)
	if a.Cmp(&work.left) <= 0 {
		work.profitAt(net, minimum, a, b, c)
		input.Set(minimum)
		return nil
	}
	optimum := &work.optimum
	if root == nil {
		work.left.Mul(a, b)
		optimum.Sqrt(&work.left)
	} else {
		if root.Sign() == 0 {
			work.left.Mul(a, b)
			root.Sqrt(&work.left)
		}
		optimum.Set(root)
	}
	optimum.Sub(optimum, b)
	optimum.Div(optimum, c)
	if optimum.Cmp(minimum) < 0 {
		optimum.Set(minimum)
	}
	if optimum.Cmp(maximum) > 0 {
		optimum.Set(maximum)
	}
	best, profit := &work.best, &work.profit
	best.Set(optimum)
	work.profitAt(profit, optimum, a, b, c)
	work.next.Add(optimum, big.NewInt(1))
	for _, candidate := range [3]*big.Int{minimum, maximum, &work.next} {
		if candidate.Cmp(maximum) > 0 {
			continue
		}
		value := &work.value
		work.profitAt(value, candidate, a, b, c)
		if value.Cmp(profit) > 0 || (value.Cmp(profit) == 0 && candidate.Cmp(best) < 0) {
			best.Set(candidate)
			profit.Set(value)
		}
	}
	input.Set(best)
	net.Set(profit)
	return nil
}

type cycleWorkspace struct {
	a, b, c, fee, legA, legB, left, right big.Int
	optimum, best, profit, next, value    big.Int
}

var cycleWorkspaces = sync.Pool{New: func() any { return new(cycleWorkspace) }}

func (w *cycleWorkspace) profitAt(result, amount, a, b, c *big.Int) {
	w.left.Mul(c, amount)
	w.left.Add(&w.left, b)
	w.right.Mul(a, amount)
	result.Div(&w.right, &w.left)
	result.Sub(result, amount)
}

func (w *cycleWorkspace) release() {
	// Do not retain unusually large buffers from callers outside the runtime's
	// 256-bit reserves / 16-hop search. Ordinary words stay allocated for reuse.
	for _, value := range [...]*big.Int{&w.a, &w.b, &w.c, &w.fee, &w.legA, &w.legB, &w.left, &w.right, &w.optimum, &w.best, &w.profit, &w.next, &w.value} {
		if cap(value.Bits()) > 512 {
			*value = big.Int{}
		}
	}
	cycleWorkspaces.Put(w)
}
