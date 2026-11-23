package pipeline

import "math/big"

type EconomicsStatus string

const (
	NetUnknown      EconomicsStatus = "unknown"
	NetNonPositive  EconomicsStatus = "non_positive"
	NetBelowMinimum EconomicsStatus = "below_minimum"
	NetEligible     EconomicsStatus = "eligible"
)

// Classify only after every cost and quote-calibration prerequisite is met.
// A partially populated estimated_net remains unknown, even if it is negative.
func (e *ResearchEconomics) classify(minimum *big.Int) {
	e.Eligible = false
	switch {
	case e.Net == nil:
		e.Status = NetUnknown
	case e.Net.Sign() <= 0:
		e.Status = NetNonPositive
	case e.Net.Cmp(minimum) < 0:
		e.Status = NetBelowMinimum
	default:
		e.Status = NetEligible
		e.Eligible = true
	}
}
