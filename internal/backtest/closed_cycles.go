package backtest

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/ingest"
)

// PoolMetadata describes immutable currency identity, never historical state.
type PoolMetadata struct {
	Key            domain.PoolKey
	Token0, Token1 domain.Address
	Created        uint64
}
type ClosedCycle struct {
	Fees *domain.ReceiptFees `json:"paid_fees,omitempty"`
	KnownRoute
	Transaction domain.Hash `json:"transaction"`
	SwapInputs  []*big.Int  `json:"swap_inputs"`
	ActualGross *big.Int    `json:"actual_swap_gross"`
}

// SwapDecoder supplies protocol parsing without coupling replay analysis to adapters.
type SwapDecoder struct {
	Matches func(domain.Hash) bool
	Amounts func(domain.PoolKind, []domain.Hash, []byte) (*big.Int, *big.Int, bool, bool)
}
type swapEdge struct {
	key           domain.PoolKey
	in, out       domain.Address
	input, output *big.Int
}

// ExtractClosedCycle recognizes a single simple supported swap cycle with one
// surplus currency. Receipt callback order may be reversed; token flow orders
// the route. Funding, settlement, gas and unsupported protocols remain outside
// this denominator. The reason is empty only for a recognized cycle.
func ExtractClosedCycle(ref domain.BlockRef, tx ingest.Transaction, metadata []PoolMetadata, decoder SwapDecoder) (ClosedCycle, string) {
	out := ClosedCycle{Transaction: tx.Hash, Fees: tx.Fees}
	if !tx.Successful {
		return out, "failed_transaction"
	}
	var edges []swapEdge
	pools := map[domain.PoolKey]bool{}
	for _, log := range tx.Logs {
		if len(log.Topics) == 0 || !decoder.Matches(log.Topics[0]) {
			continue
		}
		var match *PoolMetadata
		for i := range metadata {
			m := &metadata[i]
			if m.Key.Address != log.Address || m.Created > ref.Number {
				continue
			}
			if m.Key.IsV4() && (len(log.Topics) < 2 || log.Topics[1] != m.Key.PoolID) {
				continue
			}
			if match != nil && *match != *m {
				return out, "ambiguous_pool_metadata"
			}
			match = m
		}
		if match == nil {
			return out, "unknown_swap_pool"
		}
		input, output, forward, matched := decoder.Amounts(match.Key.Kind, log.Topics, log.Data)
		if !matched || input == nil {
			return out, "unsupported_or_ambiguous_amounts"
		}
		if pools[match.Key] {
			return out, "repeated_pool"
		}
		pools[match.Key] = true
		in, to := match.Token0, match.Token1
		if !forward {
			in, to = to, in
		}
		edges = append(edges, swapEdge{match.Key, in, to, input, output})
		if len(edges) > 16 {
			return out, "too_many_swaps"
		}
	}
	if len(edges) < 2 {
		return out, "not_multi_swap"
	}
	next := map[domain.Address]swapEdge{}
	incoming := map[domain.Address]bool{}
	balance := map[domain.Address]*big.Int{}
	for _, e := range edges {
		if _, ok := next[e.in]; ok || incoming[e.out] || e.in == e.out {
			return out, "non_simple_flow"
		}
		next[e.in] = e
		incoming[e.out] = true
		if balance[e.in] == nil {
			balance[e.in] = new(big.Int)
		}
		if balance[e.out] == nil {
			balance[e.out] = new(big.Int)
		}
		balance[e.in].Sub(balance[e.in], e.input)
		balance[e.out].Add(balance[e.out], e.output)
	}
	var start domain.Address
	var gross *big.Int
	positives := 0
	deficit := false
	for token, amount := range balance {
		deficit = deficit || amount.Sign() < 0
		if amount.Sign() > 0 {
			positives++
			start = token
			gross = amount
		}
	}
	// Reason precedence must not depend on map iteration order.
	if deficit {
		return out, "flow_deficit"
	}
	if positives > 1 {
		return out, "multiple_surplus_currencies"
	}
	if gross == nil {
		return out, "no_swap_surplus"
	}
	cycle := graph.Cycle{Start: start, Len: len(edges)}
	token := start
	var tokens []domain.Address
	for range len(edges) {
		e, ok := next[token]
		if !ok {
			return out, "open_flow"
		}
		cycle.Pools = append(cycle.Pools, e.key)
		tokens = append(tokens, token)
		out.SwapInputs = append(out.SwapInputs, new(big.Int).Set(e.input))
		delete(next, token)
		token = e.out
	}
	if token != start || len(next) > 0 {
		return out, "disconnected_flow"
	}
	out.KnownRoute = KnownRoute{ID: tx.Hash.Hex(), BlockHash: ref.Hash, BeforeTx: tx.Index, Cycle: cycle, Input: edgesInput(edges, start), Tokens: tokens}
	out.ActualGross = new(big.Int).Set(gross)
	return out, ""
}
func edgesInput(edges []swapEdge, start domain.Address) *big.Int {
	for _, e := range edges {
		if e.in == start {
			return new(big.Int).Set(e.input)
		}
	}
	return nil
}
