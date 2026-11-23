package slipstream

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func feeTestState() pool.State {
	return pool.State{
		Key:          domain.PoolKey{Address: domain.Address{0xbb}, Kind: domain.PoolKindSlipstream},
		Token0:       domain.Address{0x01},
		Token1:       domain.Address{0x02},
		Fee:          500,
		TickSpacing:  10,
		SqrtPriceX96: q96(),
		Tick:         7,
		Liquidity:    big.NewInt(1_000),
		UpdatedBlock: 42,
	}
}

func TestLoadFeeUpdatesOnlyFee(t *testing.T) {
	caller := newFakeCaller()
	caller.setOut(selectorBytes(t, SelectorFee), feeResult(3000))
	state := feeTestState()

	updated, err := LoadFee(context.Background(), caller, state, "0x10")
	if err != nil {
		t.Fatalf("LoadFee: %v", err)
	}
	if updated.Fee != 3000 {
		t.Fatalf("fee = %d, want 3000", updated.Fee)
	}
	if updated.Key != state.Key || updated.TickSpacing != state.TickSpacing ||
		updated.Tick != state.Tick || updated.UpdatedBlock != state.UpdatedBlock {
		t.Fatalf("identity or price fields changed: %+v", updated)
	}
	if updated.SqrtPriceX96.Cmp(state.SqrtPriceX96) != 0 || updated.Liquidity.Cmp(state.Liquidity) != 0 {
		t.Fatalf("price or liquidity changed")
	}
	if len(caller.calls) != 1 || caller.calls[0].block != "0x10" {
		t.Fatalf("calls = %+v, want one fee() at 0x10", caller.calls)
	}
}

func TestFeeLoaderMethodForm(t *testing.T) {
	caller := newFakeCaller()
	caller.setOut(selectorBytes(t, SelectorFee), feeResult(100))
	updated, err := (FeeLoader{Caller: caller}).LoadFee(context.Background(), feeTestState(), "0x20")
	if err != nil {
		t.Fatalf("LoadFee: %v", err)
	}
	if updated.Fee != 100 {
		t.Fatalf("fee = %d, want 100", updated.Fee)
	}
}

func TestLoadFeeRejections(t *testing.T) {
	feeErr := errors.New("fee unavailable")
	tests := []struct {
		name   string
		mutate func(*pool.State)
		caller pool.ContractCaller
		block  string
		want   error
	}{
		{
			name:   "wrong kind",
			mutate: func(s *pool.State) { s.Key.Kind = domain.PoolKindV3 },
			want:   ErrInvalidSpec,
		},
		{
			name:   "zero address",
			mutate: func(s *pool.State) { s.Key.Address = domain.Address{} },
			want:   ErrInvalidSpec,
		},
		{
			name:   "nil caller",
			caller: nil,
			want:   ErrInvalidSpec,
		},
		{
			name:  "empty block",
			block: "",
			want:  ErrInvalidSpec,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := feeTestState()
			if test.mutate != nil {
				test.mutate(&state)
			}
			caller := test.caller
			if caller == nil && test.name != "nil caller" {
				caller = newFakeCaller()
			}
			block := test.block
			if block == "" && test.name != "empty block" {
				block = "0x10"
			}
			if _, err := LoadFee(context.Background(), caller, state, block); !errors.Is(err, test.want) {
				t.Fatalf("LoadFee error = %v, want %v", err, test.want)
			}
		})
	}

	t.Run("fee failure", func(t *testing.T) {
		caller := newFakeCaller()
		caller.setError(selectorBytes(t, SelectorFee), feeErr)
		if _, err := LoadFee(context.Background(), caller, feeTestState(), "0x10"); !errors.Is(err, feeErr) {
			t.Fatalf("LoadFee error = %v, want %v", err, feeErr)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := LoadFee(ctx, newFakeCaller(), feeTestState(), "0x10"); !errors.Is(err, context.Canceled) {
			t.Fatalf("LoadFee error = %v, want context.Canceled", err)
		}
	})
}

func TestLoadFeeDoesNotReuseUnidentifiedOracleBranch(t *testing.T) {
	state := dynamicPool()
	caller := newFakeCaller()
	caller.setOut(feeCall, "0x"+strings.Repeat("0", 61)+"1f4")
	updated, err := LoadFee(t.Context(), caller, state, "0xb")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Fee != 500 || updated.Slipstream != nil || len(caller.calls) != 1 {
		t.Fatal("new block retained old oracle", updated, len(caller.calls))
	}
	before := len(caller.calls)
	updated, err = LoadFee(t.Context(), caller, state, "0xa")
	if err != nil || updated.Slipstream != nil || updated.Fee != 500 || len(caller.calls) != before+1 {
		t.Fatal("block number alone reused an oracle from a potentially different branch", err)
	}
}
