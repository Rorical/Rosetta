package pipeline

import (
	"context"
	"errors"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"golang.org/x/crypto/sha3"
)

type SwapAuditPolicy struct {
	MaxSwaps     int    `json:"max_swaps"`
	EvaluationMS int64  `json:"evaluation_ms"`
	EveryBlocks  uint64 `json:"every_blocks"`
}

type SwapComparison struct {
	Explanation   string         `json:"explanation,omitempty"`
	Requoted      *big.Int       `json:"fee_requoted_output,omitempty"`
	RequotedDelta *big.Int       `json:"fee_requoted_delta,omitempty"`
	RefreshedFee  *uint32        `json:"refreshed_fee_pips,omitempty"`
	Pool          domain.PoolKey `json:"pool"`
	Transaction   domain.Hash    `json:"transaction"`
	LogIndex      int            `json:"receipt_log_index"`
	Status        string         `json:"status"`
	TokenIn       domain.Address `json:"token_in"`
	Input         *big.Int       `json:"input,omitempty"`
	Observed      *big.Int       `json:"observed_output,omitempty"`
	Quoted        *big.Int       `json:"quoted_output,omitempty"`
	Delta         *big.Int       `json:"output_delta,omitempty"`
	Fee           uint32         `json:"fee_pips"`
	DynamicFee    bool           `json:"dynamic_fee"`
	Reason        string         `json:"reason,omitempty"`
}

type SwapAuditResult struct {
	Comparisons []SwapComparison `json:"comparisons"`
	Limited     bool             `json:"sample_limit_reached"`
	// Receipt amounts cannot identify exact-input versus exact-output intent,
	// nor prove the funding or settlement of a hypothetical atomic route.
	Basis string `json:"basis"`
}

var auditSwapV2 = auditTopic("Swap(address,uint256,uint256,uint256,uint256,address)")
var auditSwapVelodrome = auditTopic("Swap(address,address,uint256,uint256,uint256,uint256)")
var auditSwapV4 = auditTopic("Swap(bytes32,address,int128,int128,uint160,uint128,int24,uint24)")
var errSwapAuditLimit = errors.New("swap audit sample limit")

func auditTopic(signature string) (hash domain.Hash) {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(signature))
	copy(hash[:], h.Sum(nil))
	return
}

// AuditSwaps compares local exact-input math with real EVM receipt amounts at
// the same pre-swap position. Replaying every intervening log also covers mint,
// burn, multiple swaps in one transaction and Camelot fee events. No RPC is used.
func AuditSwaps(ctx context.Context, base poolstate.View, ref domain.BlockRef, transactions []ingest.Transaction, limit int, fees ...pool.State) (SwapAuditResult, error) {
	result := SwapAuditResult{Basis: "receipt_amount_pair; swap_intent_funding_and_atomic_route_unverified"}
	type synced struct {
		before, after pool.State
		tx            domain.Hash
	}
	feeStates := map[domain.PoolKey]pool.State{}
	for _, p := range fees {
		feeStates[p.Key] = p
	}
	reserves := make(map[domain.PoolKey]synced)
	err := ingest.ReplayLogs(ctx, base, ref, transactions, func(ctx context.Context, tx domain.Hash, index int, log ingest.Log, key domain.PoolKey, view poolstate.View) error {
		if len(log.Topics) == 0 {
			return nil
		}
		p, _ := view.Pool(key)
		if key.Kind == domain.PoolKindV2 || key.Kind == domain.PoolKindVelodromeV2 {
			r0, r1, sync, err := uniswapv2.DecodeSync(log.Topics, log.Data)
			if key.Kind == domain.PoolKindVelodromeV2 {
				r0, r1, sync, err = velodromev2.DecodeSync(log.Topics, log.Data)
			}
			if err == nil && sync {
				after := p
				after.Reserve0, after.Reserve1 = r0, r1
				reserves[key] = synced{before: p, after: after, tx: tx}
				return nil
			}
		}
		input, output, zeroForOne, swap := auditAmounts(log, key.Kind)
		if !swap {
			return nil
		}
		if len(result.Comparisons) >= limit {
			result.Limited = true
			return errSwapAuditLimit
		}
		row := SwapComparison{Pool: key, Transaction: tx, LogIndex: index, Input: input, Observed: output, Status: "unavailable", DynamicFee: DynamicFee(key.Kind)}
		if input == nil {
			row.Reason = "ambiguous_swap_amounts"
		} else if key.Kind == domain.PoolKindV2 || key.Kind == domain.PoolKindVelodromeV2 {
			sync, ok := reserves[key]
			delete(reserves, key)
			if !ok || sync.tx != tx || !auditReserveTransition(sync.before, sync.after, input, output, zeroForOne) {
				row.Reason = "missing_or_non_swap_reserve_transition"
			} else {
				p = sync.before
			}
		}
		row.Fee = p.Fee
		row.TokenIn = p.Token0
		if !zeroForOne {
			row.TokenIn = p.Token1
			if p.FeeOneForZero != nil {
				row.Fee = *p.FeeOneForZero
			}
		}
		if fee, err := dex.SwapFee(p, row.TokenIn); err == nil {
			row.Fee = fee
		}
		if row.Reason == "" {
			adapter, err := dex.NewAdapter(p)
			if err == nil {
				deep, _ := view.Deep(key)
				row.Quoted, err = adapter.QuoteExactIn(ctx, p, deep, row.TokenIn, input)
			}
			if err != nil {
				row.Reason = err.Error()
			} else {
				row.Delta = new(big.Int).Sub(row.Quoted, output)
				row.Status = "different"
				if row.Delta.Sign() == 0 {
					row.Status = "matched"
				} else {
					explainSwapDifference(ctx, &row, p, view, feeStates)
				}
			}
		}
		result.Comparisons = append(result.Comparisons, row)
		return ctx.Err()
	})
	if errors.Is(err, errSwapAuditLimit) {
		err = nil
	}
	return result, err
}

func auditReserveTransition(before, after pool.State, input, output *big.Int, zeroForOne bool) bool {
	if before.Reserve0 == nil || before.Reserve1 == nil {
		return false
	}
	in, out, afterIn, afterOut := before.Reserve0, before.Reserve1, after.Reserve0, after.Reserve1
	if !zeroForOne {
		in, out, afterIn, afterOut = out, in, afterOut, afterIn
	}
	credited := new(big.Int).Set(input)
	if before.Key.Kind == domain.PoolKindVelodromeV2 {
		// Velodrome transfers the floored input fee to its fee collector
		// before Sync. Uniswap V2 retains that fee in the reserves.
		fee := new(big.Int).Mul(input, new(big.Int).SetUint64(uint64(before.Fee/100)))
		fee.Quo(fee, big.NewInt(10000))
		credited.Sub(credited, fee)
	}
	return new(big.Int).Add(in, credited).Cmp(afterIn) == 0 && new(big.Int).Sub(out, output).Cmp(afterOut) == 0
}

func auditAmounts(log ingest.Log, kind domain.PoolKind) (input, output *big.Int, zeroForOne, swap bool) {
	return dex.DecodeSwapAmounts(kind, log.Topics, log.Data)
}
