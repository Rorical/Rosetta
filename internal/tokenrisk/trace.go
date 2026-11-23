package tokenrisk

import (
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
)

// CallFrame consumes ordinary callTracer output. Delegate calls are traversed
// but never counted as separate token transfers.
type CallFrame struct {
	Type   string      `json:"type"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	Input  string      `json:"input"`
	Output string      `json:"output"`
	Error  string      `json:"error"`
	Calls  []CallFrame `json:"calls"`
}
type balanceKey struct{ token, account domain.Address }
type balanceRead struct {
	amount *big.Int
	index  int
}

// Detect requires two direct balance reads around exactly one successful
// incoming transfer and no outgoing transfer. A reverted enclosing executor
// still supplies valid intermediate balances; a reverted child supplies none.
// Only holder's reads of its own balance count: a trace also runs code the
// route does not trust (hooks, unmodelled pools), which could stage a read
// around a self-transfer to fake a shortfall of any token.
func Detect(frame CallFrame, block domain.BlockRef, holder domain.Address) []Evidence {
	var out []Evidence
	seen := make(map[domain.Address]bool)
	var visit func(CallFrame)
	visit = func(f CallFrame) {
		balances := make(map[balanceKey]balanceRead)
		for i, c := range f.Calls {
			key, value, ok := readBalance(c)
			if !ok || key.account != holder || !strings.EqualFold(c.From, holder.Hex()) {
				continue
			}
			if before, exists := balances[key]; exists && !seen[key.token] {
				count := 0
				ambiguous := false
				var requested *big.Int
				for _, between := range f.Calls[before.index+1 : i] {
					scanTransfers(between, key, &count, &requested, &ambiguous)
				}
				received := new(big.Int).Sub(value, before.amount)
				if count == 1 && !ambiguous && received.Sign() >= 0 && received.Cmp(requested) < 0 {
					out = append(out, Evidence{Token: key.token, Requested: requested, Received: received, Block: block, Source: "executor_trace"})
					seen[key.token] = true
				}
			}
			balances[key] = balanceRead{value, i}
		}
		for _, c := range f.Calls {
			visit(c)
		}
	}
	visit(frame)
	return out
}
func readBalance(f CallFrame) (balanceKey, *big.Int, bool) {
	if f.Error != "" || f.Type != "STATICCALL" || len(f.Input) != 74 || !strings.HasPrefix(f.Input, "0x70a08231") || len(f.Output) != 66 {
		return balanceKey{}, nil, false
	}
	token, e1 := domain.ParseAddress(f.To)
	account, e2 := domain.ParseAddress("0x" + f.Input[34:74])
	value, ok := new(big.Int).SetString(f.Output[2:], 16)
	return balanceKey{token, account}, value, ok && e1 == nil && e2 == nil
}
func scanTransfers(f CallFrame, key balanceKey, count *int, requested **big.Int, ambiguous *bool) {
	if f.Error != "" {
		return
	}
	if f.Type == "CALL" && strings.EqualFold(f.To, key.token.Hex()) {
		var from, to domain.Address
		var amount *big.Int
		var err error
		switch {
		case len(f.Input) == 138 && strings.HasPrefix(f.Input, "0xa9059cbb"):
			from, err = domain.ParseAddress(f.From)
			if err != nil {
				return
			}
			to, err = domain.ParseAddress("0x" + f.Input[34:74])
			amount, _ = new(big.Int).SetString(f.Input[74:], 16)
		case len(f.Input) == 202 && strings.HasPrefix(f.Input, "0x23b872dd"):
			from, err = domain.ParseAddress("0x" + f.Input[34:74])
			if err != nil {
				return
			}
			to, err = domain.ParseAddress("0x" + f.Input[98:138])
			amount, _ = new(big.Int).SetString(f.Input[138:], 16)
		default:
			*ambiguous = true
			return
		}
		if err != nil || amount == nil {
			*ambiguous = true
			return
		}
		if from == key.account {
			*ambiguous = true
		}
		if to == key.account {
			(*count)++
			*requested = amount
		}
		return
	}
	for _, c := range f.Calls {
		scanTransfers(c, key, count, requested, ambiguous)
	}
}
