package tokenrisk

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Holds reports whether holder's balance of token covers amount at the
// traced block.
type Holds func(ctx context.Context, token, holder domain.Address, amount *big.Int) (bool, error)

// Refusal scopes which reverted transfers count. The trace includes code the
// route does not trust (hooks, unmodelled pools), which can fail a transfer of
// any token on purpose; only a route pool paying the executor counts, and
// protected tokens (start assets) are never recorded, so a griefer can at
// most close routes through an intermediate token.
type Refusal struct {
	Executor  domain.Address
	Payers    map[domain.Address]bool
	Protected map[domain.Address]bool
	Holds     Holds
}

// DetectRefused finds token transfers that reverted although the payer held
// the amount: the token refused the transfer itself. Some tokens (ZWSYD on
// Robinhood) revert every pool transfer to an executor inside a swap, while
// transfers to a plain address pass. A payer short of the amount is a stale
// quote or a drained lender, not evidence against the token. Only a plain
// revert counts: out-of-gas is the caller's gas, not the token's choice. The
// payer's balance is read at a block boundary, and a pool can move its own
// balance away within the transaction, so a revert that reports insufficient
// balance never counts whatever the boundary balance was.
func DetectRefused(ctx context.Context, frame CallFrame, block domain.BlockRef, scope Refusal) ([]Evidence, error) {
	var out []Evidence
	seen := map[domain.Address]bool{}
	var visit func(CallFrame) error
	visit = func(f CallFrame) error {
		for _, c := range f.Calls {
			if err := visit(c); err != nil {
				return err
			}
		}
		if f.Type != "CALL" || f.Error != "execution reverted" || len(f.Input) != 138 || !strings.HasPrefix(f.Input, "0xa9059cbb") {
			return nil
		}
		token, err := domain.ParseAddress(f.To)
		if err != nil || seen[token] || scope.Protected[token] {
			return nil
		}
		payer, err := domain.ParseAddress(f.From)
		if err != nil || !scope.Payers[payer] {
			return nil
		}
		recipient, err := domain.ParseAddress("0x" + f.Input[34:74])
		if err != nil || recipient != scope.Executor {
			return nil
		}
		amount, ok := new(big.Int).SetString(f.Input[74:], 16)
		if !ok || amount.Sign() <= 0 {
			return nil
		}
		if insufficientBalance(f.Output, payer, amount) {
			return nil
		}
		covered, err := scope.Holds(ctx, token, payer, amount)
		if err != nil || !covered {
			return err
		}
		seen[token] = true
		out = append(out, Evidence{Token: token, Requested: amount, Received: new(big.Int), Block: block, Source: "executor_trace"})
		return nil
	}
	return out, visit(frame)
}

// insufficientBalance recognises the common ERC-20 shortfall reverts: an
// OpenZeppelin 5 ERC20InsufficientBalance naming payer with less than amount,
// the OpenZeppelin 4 message, and Solidity 0.8 arithmetic underflow
// Panic(0x11). The token writes this data, so a trap can hide behind it; that
// only stops it being learned here, because its route still fails preflight
// and is never sent.
func insufficientBalance(output string, payer domain.Address, amount *big.Int) bool {
	data, err := hex.DecodeString(strings.TrimPrefix(output, "0x"))
	if err != nil || len(data) < 4 {
		return false
	}
	switch hex.EncodeToString(data[:4]) {
	case "e450d38c": // ERC20InsufficientBalance(address sender, uint256 balance, uint256 needed)
		if len(data) != 100 {
			return false
		}
		var sender domain.Address
		copy(sender[:], data[16:36])
		return sender == payer && new(big.Int).SetBytes(data[36:68]).Cmp(amount) < 0
	case "4e487b71":
		return len(data) == 36 && new(big.Int).SetBytes(data[4:]).Int64() == 0x11
	case "08c379a0":
		return strings.Contains(string(data[4:]), "ERC20: transfer amount exceeds balance")
	}
	return false
}
