package wrap

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

var wrapped = domain.Address{0x82, 0xaf}

func TestQuoteIsTheInputInBothDirections(t *testing.T) {
	state := State(wrapped)
	if state.Token0 != (domain.Address{}) || state.Token1 != wrapped || state.Key != Key(wrapped) || state.Fee != 0 {
		t.Fatalf("state %+v", state)
	}
	adapter := New()
	if adapter.Kind() != domain.PoolKindWrap || !adapter.ExactInputMonotone(state) {
		t.Fatal("kind or monotonicity")
	}
	huge := new(big.Int).Lsh(big.NewInt(1), 255)
	for _, tokenIn := range []domain.Address{{}, wrapped} {
		for _, amount := range []*big.Int{big.NewInt(1), big.NewInt(1_000_000_000_000_000_000), huge} {
			out, err := adapter.QuoteExactIn(t.Context(), state, pool.DeepState{}, tokenIn, amount)
			if err != nil {
				t.Fatal(err)
			}
			if out.Cmp(amount) != 0 {
				t.Fatalf("%s in, %s out", amount, out)
			}
			if out == amount {
				t.Fatal("quote aliases its input")
			}
		}
	}
}

func TestQuoteRejectsOtherTokensAmountsAndPools(t *testing.T) {
	adapter := New()
	one := big.NewInt(1)
	tests := []struct {
		name    string
		state   pool.State
		tokenIn domain.Address
		amount  *big.Int
	}{
		{"foreign token", State(wrapped), domain.Address{9}, one},
		{"nil amount", State(wrapped), wrapped, nil},
		{"zero amount", State(wrapped), wrapped, new(big.Int)},
		{"negative amount", State(wrapped), wrapped, big.NewInt(-1)},
		{"another kind", pool.State{Key: domain.PoolKey{Address: wrapped, Kind: domain.PoolKindV2}, Token1: wrapped}, wrapped, one},
		{"zero address", pool.State{Key: domain.PoolKey{Kind: domain.PoolKindWrap}}, domain.Address{}, one},
		{"token0 not native", pool.State{Key: Key(wrapped), Token0: domain.Address{9}, Token1: wrapped}, wrapped, one},
		{"token1 not the target", pool.State{Key: Key(wrapped), Token1: domain.Address{9}}, domain.Address{}, one},
		{"fee", pool.State{Key: Key(wrapped), Token1: wrapped, Fee: 1}, wrapped, one},
		{"pool ID", pool.State{Key: domain.PoolKey{Address: wrapped, Kind: domain.PoolKindWrap, PoolID: domain.Hash{1}}, Token1: wrapped}, wrapped, one},
		{"hooks", pool.State{Key: domain.PoolKey{Address: wrapped, Kind: domain.PoolKindWrap, Hooks: domain.Address{1}}, Token1: wrapped}, wrapped, one},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if out, err := adapter.QuoteExactIn(t.Context(), test.state, pool.DeepState{}, test.tokenIn, test.amount); !errors.Is(err, pool.ErrInvalidState) || out != nil {
				t.Fatalf("quoted %v, %v", out, err)
			}
		})
	}
}

// The screening curve must not show slippage at any amount a route moves:
// a constant-product output on these reserves is within one unit of the input.
func TestRankingModelIsFlat(t *testing.T) {
	model, err := RankingModel(State(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	if model.ConstantProduct || model.ReverseIn != nil || model.Reserve0.Cmp(model.Reserve1) != 0 || model.Reserve0 == model.Reserve1 {
		t.Fatalf("model %+v", model)
	}
	amount := new(big.Int).Lsh(big.NewInt(1), 96)
	out := new(big.Int).Mul(amount, model.Reserve1)
	out.Quo(out, new(big.Int).Add(model.Reserve0, amount))
	if new(big.Int).Sub(amount, out).Cmp(big.NewInt(1)) > 0 {
		t.Fatalf("curve loses %s of %s", new(big.Int).Sub(amount, out), amount)
	}
	if _, err := RankingModel(pool.State{Key: Key(wrapped), Token1: domain.Address{9}}); !errors.Is(err, pool.ErrInvalidState) {
		t.Fatal("ranked a malformed pool", err)
	}
}
