package paperreport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

type LiveAssetReport struct {
	Cancelled int `json:"cancelled"`
	// Maintenance counts owner sweeps and unwraps; their fees are costs, not
	// arbitrage outcomes.
	Maintenance int      `json:"maintenance,omitempty"`
	Asset       string   `json:"asset"`
	Success     int      `json:"success"`
	Reverted    int      `json:"reverted"`
	Profit      *big.Int `json:"profit_after_loan"`
	Net         *big.Int `json:"net_at_admission_conversion"`
}
type LiveReport struct {
	Basis                string            `json:"basis"`
	ShadowIntents        int               `json:"shadow_intents"`
	Submitted            int               `json:"unique_submitted_hashes"`
	Settled              int               `json:"unique_settled_hashes"`
	DuplicateSettlements int               `json:"duplicate_settlements"`
	NativeFees           *big.Int          `json:"actual_native_fees"`
	Assets               []LiveAssetReport `json:"assets"`
}

// ReadLive counts only canonical paid settlements. Simulations and shadow
// intentions cannot enter the profit denominator. Recovered rows deduplicate
// by transaction hash; asset values are never summed across denominations.
func ReadLive(ctx context.Context, input io.Reader, limit int) (LiveReport, error) {
	out := LiveReport{Basis: "canonical_paid_receipts; token_net_uses_admission_conversion", NativeFees: new(big.Int)}
	if limit < 1 || limit > 200000 {
		return out, fmt.Errorf("invalid live report limit")
	}
	submitted := map[domain.Hash]bool{}
	settled := map[domain.Hash]bool{}
	assets := map[domain.Address]*LiveAssetReport{}
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 4<<20)
	var chain domain.ChainIdentity
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var row struct {
			Type        string                `json:"type"`
			Mode        string                `json:"mode"`
			Transaction domain.Hash           `json:"transaction"`
			Receipt     *execution.Settlement `json:"receipt"`
		}
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			return out, fmt.Errorf("decode reactive journal: %w", err)
		}
		switch row.Type {
		case "reactive_intent":
			if row.Mode == "shadow" {
				out.ShadowIntents++
			} else if row.Mode == "live" && row.Transaction != (domain.Hash{}) {
				submitted[row.Transaction] = true
			}
		case "maintenance":
			// A refill attempt; its fee arrives as a settlement row.
		case "settlement":
			r := row.Receipt
			if r != nil && r.FeeStatus != "" {
				continue // no paid fee yet, or none ever came
			}
			if r == nil || r.Hash == (domain.Hash{}) || r.Intent == nil || r.Fees == nil || r.Fees.Total == nil || r.Profit == nil || r.Net == nil {
				return out, fmt.Errorf("incomplete paid settlement")
			}
			if chain.ChainID == 0 {
				chain = r.Intent.Chain
			} else if chain != r.Intent.Chain {
				return out, fmt.Errorf("mixed chains in live journal")
			}
			if settled[r.Hash] {
				out.DuplicateSettlements++
				continue
			}
			settled[r.Hash] = true
			out.NativeFees.Add(out.NativeFees, r.Fees.Total)
			a := assets[r.Asset]
			if a == nil {
				a = &LiveAssetReport{Asset: r.Asset.Hex(), Profit: new(big.Int), Net: new(big.Int)}
				assets[r.Asset] = a
			}
			if r.Maintenance != "" {
				a.Maintenance++
			} else if r.Cancelled {
				a.Cancelled++
			} else if r.Success {
				a.Success++
			} else {
				a.Reverted++
			}
			a.Profit.Add(a.Profit, r.Profit)
			a.Net.Add(a.Net, r.Net)
		default:
			return out, fmt.Errorf("unsupported reactive journal row %q", row.Type)
		}
		if len(submitted)+len(settled) > limit {
			return out, fmt.Errorf("live report sample limit exceeded")
		}
	}
	if err := scan.Err(); err != nil {
		return out, err
	}
	out.Submitted = len(submitted)
	out.Settled = len(settled)
	for _, a := range assets {
		out.Assets = append(out.Assets, *a)
	}
	sort.Slice(out.Assets, func(i, j int) bool { return out.Assets[i].Asset < out.Assets[j].Asset })
	return out, nil
}
