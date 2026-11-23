package remote

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type paperFeeChange struct {
	Pool      domain.PoolKey `json:"pool"`
	Cached    uint32         `json:"cached_pips"`
	Execution uint32         `json:"execution_pips"`
}
type paperFeeDiagnostic struct {
	Trigger       string           `json:"trigger,omitempty"`
	ExecutedGross *big.Int         `json:"executed_gross,omitempty"`
	Residual      *big.Int         `json:"requote_minus_executed_gross,omitempty"`
	Basis         string           `json:"basis"`
	Changes       []paperFeeChange `json:"changes,omitempty"`
	Gross         *big.Int         `json:"requote_gross,omitempty"`
	Legs          []arb.Leg        `json:"requote_legs,omitempty"`
	Error         string           `json:"error,omitempty"`
}

func paperFeeDiagnosisNeeded(job paperExecutionJob, result paperExecutionResult) bool {
	if job.delayMS != 0 {
		return false
	}
	if result.Status == "reverted" {
		return true
	}
	if result.Status != "success" || result.Profit == nil || result.LoanFee == nil || job.opportunity.Net == nil {
		return false
	}
	// Compare pre-loan gross. A repaid loan or an integer-unit rounding
	// difference must not spend extra RPC reads on fee diagnostics.
	actual := new(big.Int).Add(result.Profit, result.LoanFee)
	difference := new(big.Int).Sub(job.opportunity.Net, actual)
	difference.Abs(difference)
	return difference.Cmp(big.NewInt(1)) > 0 && difference.Mul(difference, big.NewInt(100)).Cmp(job.opportunity.Net) >= 0
}

func attachPaperFeeDiagnostic(ctx context.Context, s *snapshot, job paperExecutionJob, result *paperExecutionResult, sender domain.Address) {
	if !paperFeeDiagnosisNeeded(job, *result) {
		return
	}
	diagnosticCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	diagnostic := diagnosePaperFees(diagnosticCtx, s, job.event.Job.View, job.opportunity, sender)
	if diagnostic == nil {
		return
	}
	diagnostic.Trigger = "revert"
	if result.Status == "success" {
		diagnostic.Trigger = "gross_mismatch"
		diagnostic.ExecutedGross = new(big.Int).Add(result.Profit, result.LoanFee)
		if diagnostic.Gross != nil {
			diagnostic.Residual = new(big.Int).Sub(diagnostic.Gross, diagnostic.ExecutedGross)
		}
	}
	result.FeeDiagnostic = diagnostic
}

// Diagnose the original quote at its retained prefix. This is read-only and
// never replaces the original execution or changes its eligibility.
func diagnosePaperFees(ctx context.Context, s *snapshot, view arb.View, opportunity arb.Opportunity, sender domain.Address) *paperFeeDiagnostic {
	fees := make(map[domain.PoolKey]uint32)
	var diagnostic *paperFeeDiagnostic
	for _, key := range opportunity.Cycle.Pools {
		if key.Kind != domain.PoolKindSlipstream {
			continue
		}
		if _, seen := fees[key]; seen {
			continue
		}
		if diagnostic == nil {
			diagnostic = &paperFeeDiagnostic{Basis: "same_prefix_sender_fee_requote"}
		}
		current, ok := view.Pool(key)
		if !ok {
			diagnostic.Error = "missing original quote pool"
			return diagnostic
		}
		params := s.contractParams(key.Address, selector("fee()"), nil)
		// Dynamic modules may discount tx.origin. Match the simulated operator.
		params[0].(map[string]any)["from"] = sender.Hex()
		raw, err := s.readString(ctx, "eth_call", params)
		if err != nil {
			diagnostic.Error = fmt.Sprintf("read execution-position fee: %v", err)
			return diagnostic
		}
		decoded, err := hexData(raw)
		if err != nil || len(decoded) != 32 {
			diagnostic.Error = "execution-position fee has invalid ABI"
			return diagnostic
		}
		n := new(big.Int).SetBytes(decoded)
		if !n.IsUint64() || n.Uint64() >= 1_000_000 {
			diagnostic.Error = "execution-position fee outside pips range"
			return diagnostic
		}
		fee := uint32(n.Uint64())
		fees[key] = fee
		if fee != current.Fee {
			diagnostic.Changes = append(diagnostic.Changes, paperFeeChange{key, current.Fee, fee})
		}
	}
	if diagnostic == nil {
		return nil
	}
	quoted, err := requotePaperFees(ctx, view, opportunity, fees)
	if err != nil {
		diagnostic.Error = err.Error()
		return diagnostic
	}
	diagnostic.Gross, diagnostic.Legs = quoted.Net, quoted.Legs
	return diagnostic
}

func requotePaperFees(ctx context.Context, view arb.View, o arb.Opportunity, fees map[domain.PoolKey]uint32) (arb.Opportunity, error) {
	token := o.Cycle.Start
	amount := new(big.Int).Set(o.Input)
	quoted := arb.Opportunity{Cycle: o.Cycle, Input: new(big.Int).Set(o.Input)}
	for _, key := range o.Cycle.Pools {
		p, ok := view.Pool(key)
		if !ok {
			return arb.Opportunity{}, fmt.Errorf("fee requote pool missing: %s", key.Address)
		}
		if fee, ok := fees[key]; ok {
			p.Fee = fee
			p.Slipstream = nil // This independent EVM fee replaces the local model for diagnosis.
		}
		adapter, err := dex.NewAdapter(p)
		if err != nil {
			return arb.Opportunity{}, err
		}
		deep := pool.DeepState{}
		if key.Kind.UsesTicks() {
			deep, ok = view.Deep(key)
			if !ok {
				return arb.Opportunity{}, fmt.Errorf("fee requote tick state missing: %s", key.Address)
			}
		}
		output, err := adapter.QuoteExactIn(ctx, p, deep, token, amount)
		if err != nil {
			return arb.Opportunity{}, err
		}
		quoted.Legs = append(quoted.Legs, arb.Leg{Pool: key, AmountIn: amount, AmountOut: output})
		if token == p.Token0 {
			token = p.Token1
		} else if token == p.Token1 {
			token = p.Token0
		} else {
			return arb.Opportunity{}, fmt.Errorf("fee requote token direction differs")
		}
		amount = output
	}
	if token != o.Cycle.Start {
		return arb.Opportunity{}, fmt.Errorf("fee requote route does not close")
	}
	quoted.Output = amount
	quoted.Net = new(big.Int).Sub(amount, o.Input)
	return quoted, nil
}
