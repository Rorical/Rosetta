package tokenrisk

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func shortfall(sender domain.Address, balance, needed int64) string {
	word := func(v int64) string {
		return strings.Repeat("0", 64-len(big.NewInt(v).Text(16))) + big.NewInt(v).Text(16)
	}
	return "0xe450d38c" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(sender.Hex()), "0x") + word(balance) + word(needed)
}

func transferInput(to domain.Address, amount int64) string {
	return "0xa9059cbb" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(to.Hex()), "0x") + strings.Repeat("0", 64-len(big.NewInt(amount).Text(16))) + big.NewInt(amount).Text(16)
}

// ZWSYD (Robinhood) reverts a pool's transfer to the executor inside a swap
// although the pool holds the amount: the token refuses the
// recipient. A pool short of the amount, as with a USDG flash lender, is a
// stale quote, not the token's doing; a transfer that succeeded is neither.
// Code off the route, a transfer to someone else, an out-of-gas failure and a
// protected start asset cannot put a token on the list.
func TestDetectRefusedTransfers(t *testing.T) {
	executor, pool, short, hook := domain.Address{0xe}, domain.Address{0x01}, domain.Address{0x02}, domain.Address{0x0c}
	trap, usdg, fine, weth, other := domain.Address{0x22}, domain.Address{0x5f}, domain.Address{0x0b}, domain.Address{0x42}, domain.Address{0x33}
	frame := CallFrame{Type: "CALL", To: executor.Hex(), Error: "execution reverted", Calls: []CallFrame{
		{Type: "CALL", From: executor.Hex(), To: pool.Hex(), Error: "execution reverted", Calls: []CallFrame{
			{Type: "CALL", From: pool.Hex(), To: fine.Hex(), Input: transferInput(executor, 5)},
			{Type: "CALL", From: pool.Hex(), To: trap.Hex(), Input: transferInput(executor, 7), Error: "execution reverted"},
			{Type: "CALL", From: pool.Hex(), To: weth.Hex(), Input: transferInput(executor, 7), Error: "execution reverted"},
			{Type: "CALL", From: pool.Hex(), To: other.Hex(), Input: transferInput(hook, 7), Error: "execution reverted"},
			{Type: "CALL", From: pool.Hex(), To: other.Hex(), Input: transferInput(executor, 7), Error: "out of gas"},
			// A pool that moved its balance away first: the token reports a
			// shortfall in each common form, and none of them counts.
			{Type: "CALL", From: pool.Hex(), To: other.Hex(), Input: transferInput(executor, 7), Error: "execution reverted", Output: shortfall(pool, 3, 7)},
			{Type: "CALL", From: pool.Hex(), To: other.Hex(), Input: transferInput(executor, 7), Error: "execution reverted", Output: "0x4e487b710000000000000000000000000000000000000000000000000000000000000011"},
			{Type: "CALL", From: pool.Hex(), To: other.Hex(), Input: transferInput(executor, 7), Error: "execution reverted", Output: "0x08c379a0" + hex.EncodeToString([]byte("ERC20: transfer amount exceeds balance"))},
		}},
		{Type: "CALL", From: hook.Hex(), To: other.Hex(), Input: transferInput(executor, 7), Error: "execution reverted"},
		{Type: "CALL", From: executor.Hex(), To: short.Hex(), Error: "execution reverted", Calls: []CallFrame{
			{Type: "CALL", From: short.Hex(), To: usdg.Hex(), Input: transferInput(executor, 9), Error: "execution reverted", Output: "0x356680b7"},
		}},
	}}
	holds := func(_ context.Context, token, holder domain.Address, amount *big.Int) (bool, error) {
		return holder == pool || holder == hook, nil
	}
	scope := Refusal{Executor: executor, Payers: map[domain.Address]bool{pool: true, short: true}, Protected: map[domain.Address]bool{weth: true}, Holds: holds}
	block := domain.BlockRef{Number: 10, Hash: domain.Hash{1}}
	found, err := DetectRefused(t.Context(), frame, block, scope)
	if err != nil {
		t.Fatal(err)
	}
	// A shortfall naming another account, or a balance that covers the
	// amount, is not the payer's shortfall: the refusal still counts.
	for _, output := range []string{shortfall(short, 3, 7), shortfall(pool, 9, 7), "0x08c379a0" + hex.EncodeToString([]byte("insufficient balance for whitelist"))} {
		disguised := CallFrame{Type: "CALL", To: executor.Hex(), Calls: []CallFrame{{Type: "CALL", From: pool.Hex(), To: trap.Hex(), Input: transferInput(executor, 7), Error: "execution reverted", Output: output}}}
		if got, err := DetectRefused(t.Context(), disguised, block, scope); err != nil || len(got) != 1 {
			t.Fatalf("refusal behind %s missed: %+v %v", output[:10], got, err)
		}
	}
	if len(found) != 1 || found[0].Token != trap || found[0].Requested.Int64() != 7 || found[0].Received.Sign() != 0 || found[0].Source != "executor_trace" {
		t.Fatalf("found %+v", found)
	}
	if err := validate(found[0]); err != nil {
		t.Fatal(err)
	}
}
