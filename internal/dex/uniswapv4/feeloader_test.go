package uniswapv4

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func feeTestState() pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: poolManagerAddress,
			Kind:    domain.PoolKindV4,
			PoolID:  testPoolID,
		},
		Token0:       domain.Address{0x01},
		Token1:       domain.Address{0x02},
		Fee:          500,
		ProtocolFee:  0x07d000,
		TickSpacing:  10,
		SqrtPriceX96: q96(),
		Tick:         -100,
		Liquidity:    big.NewInt(1_000_000),
		UpdatedBlock: 42,
	}
}

func TestLoadFeeUpdatesOnlyFees(t *testing.T) {
	caller := newStateCaller()
	caller.setOut(stateViewAddress, stateViewData(SelectorGetSlot0, testPoolID), slot0Result(q96(), -100, 125, 3000))
	state := feeTestState()

	updated, err := LoadFee(context.Background(), caller, state, "0x10")
	if err != nil {
		t.Fatalf("LoadFee: %v", err)
	}
	if updated.Fee != 3000 || updated.ProtocolFee != 125 {
		t.Fatalf("fees = (%d, %d), want (3000, 125)", updated.Fee, updated.ProtocolFee)
	}
	if updated.Key != state.Key || updated.Token0 != state.Token0 || updated.Token1 != state.Token1 ||
		updated.TickSpacing != state.TickSpacing || updated.Tick != state.Tick ||
		updated.UpdatedBlock != state.UpdatedBlock {
		t.Fatalf("identity or price fields changed: %+v", updated)
	}
	if updated.SqrtPriceX96.Cmp(state.SqrtPriceX96) != 0 || updated.Liquidity.Cmp(state.Liquidity) != 0 {
		t.Fatalf("price or liquidity changed: sqrt %s liquidity %s", updated.SqrtPriceX96, updated.Liquidity)
	}
	// The caller's value is not aliased.
	state.Fee = 1
	if updated.Fee != 3000 {
		t.Fatalf("updated fee changed with the input: %d", updated.Fee)
	}

	if len(caller.calls) != 1 {
		t.Fatalf("calls = %d, want only getSlot0", len(caller.calls))
	}
	if caller.calls[0].block != "0x10" {
		t.Fatalf("call block = %q, want 0x10", caller.calls[0].block)
	}
}

func TestFeeLoaderMethodForm(t *testing.T) {
	caller := newStateCaller()
	caller.setOut(stateViewAddress, stateViewData(SelectorGetSlot0, testPoolID), slot0Result(q96(), 0, 0, 100))
	updated, err := (FeeLoader{Caller: caller}).LoadFee(context.Background(), feeTestState(), "0x20")
	if err != nil {
		t.Fatalf("LoadFee: %v", err)
	}
	if updated.Fee != 100 || updated.ProtocolFee != 0 {
		t.Fatalf("fees = (%d, %d), want (100, 0)", updated.Fee, updated.ProtocolFee)
	}
}

func TestLoadFeeRejections(t *testing.T) {
	slotErr := errors.New("slot0 unavailable")
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
			want:   ErrUnsupportedKind,
		},
		{
			name:   "zero pool id",
			mutate: func(s *pool.State) { s.Key.PoolID = domain.Hash{} },
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
				caller = newStateCaller()
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

	t.Run("slot0 failure", func(t *testing.T) {
		caller := newStateCaller()
		caller.setError(stateViewAddress, stateViewData(SelectorGetSlot0, testPoolID), slotErr)
		if _, err := LoadFee(context.Background(), caller, feeTestState(), "0x10"); !errors.Is(err, slotErr) {
			t.Fatalf("LoadFee error = %v, want %v", err, slotErr)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := LoadFee(ctx, newStateCaller(), feeTestState(), "0x10"); !errors.Is(err, context.Canceled) {
			t.Fatalf("LoadFee error = %v, want context.Canceled", err)
		}
	})
}

func TestFeeLoaderRejectsAnotherDeployment(t *testing.T) {
	caller := networkStateCaller{stateCaller: newStateCaller(), manager: domain.Address{1}, view: domain.Address{2}}
	current := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV4, Address: poolManagerAddress, PoolID: testPoolID}}
	if _, err := LoadFee(t.Context(), caller, current, "0x1"); err == nil {
		t.Fatal("fee read used another deployment's pool identity")
	}
	if len(caller.calls) != 0 {
		t.Fatal("mismatched identity issued a StateView call")
	}
}
