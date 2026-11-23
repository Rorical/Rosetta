package bidreport

import (
	execution "github.com/Rorical/Rosetta/internal/exec"
	"math/big"
)

// Economics includes every canonical paid attempt, even without a scoreable
// bid prediction. Native profit uses the frozen admission conversion, not a
// later market price. Fee coverage and profit coverage are separate.
type Economics struct {
	Paid        int      `json:"paid_attempts"`
	Success     int      `json:"successes"`
	Reverted    int      `json:"reverts"`
	Cancelled   int      `json:"cancellations"`
	ProfitKnown int      `json:"profit_known"`
	MissingFees int      `json:"missing_fees"`
	Fees        *big.Int `json:"paid_native_fees"`
	Net         *big.Int `json:"known_net_native"`
}

func (e *Economics) add(p prediction, o *execution.Settlement) {
	if e.Fees == nil {
		e.Fees, e.Net = new(big.Int), new(big.Int)
	}
	if o.FeeStatus != "" || o.Fees == nil || o.Fees.Total == nil {
		e.MissingFees++
		return
	}
	e.Paid++
	e.Fees.Add(e.Fees, o.Fees.Total)
	if o.Cancelled {
		e.Cancelled++
	} else if o.Success {
		e.Success++
	} else {
		e.Reverted++
	}
	profit := new(big.Int)
	if o.Success && !o.Cancelled {
		c := p.intent.Conversion
		if o.Profit == nil || c.Numerator == nil || c.Denominator == nil || c.Numerator.Sign() <= 0 || c.Denominator.Sign() <= 0 {
			return
		}
		profit.Mul(o.Profit, c.Denominator).Quo(profit, c.Numerator)
	}
	e.ProfitKnown++
	e.Net.Add(e.Net, profit.Sub(profit, o.Fees.Total))
}

func ageBand(age int64) string {
	switch {
	case age <= 0:
		return "unknown"
	case age < 25_000_000:
		return "0-25ms"
	case age < 50_000_000:
		return "25-50ms"
	case age < 100_000_000:
		return "50-100ms"
	case age < 200_000_000:
		return "100-200ms"
	default:
		return ">=200ms"
	}
}
