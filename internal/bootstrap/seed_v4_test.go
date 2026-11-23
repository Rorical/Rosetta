package bootstrap

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

// setV4HappyPath answers the StateView calls a V4 seed makes: slot0 with an
// lpFee of 500 and a 125/125 protocol fee, liquidity, an empty bitmap except
// for tick 0, and tick 0's liquidity.
func setV4HappyPath(t *testing.T, caller *fakeCaller, poolID domain.Hash) {
	t.Helper()
	setV4Path(t, caller, poolID, 500, 0x07d07d)
}

// setV4Path is setV4HappyPath with explicit slot0 fees, in the packed
// protocol-fee encoding and the LP-fee pips.
func setV4Path(t *testing.T, caller *fakeCaller, poolID domain.Hash, lpFee, protocolFee int64) {
	t.Helper()
	stateView := stateViewAddress(t)
	caller.setOut(stateView, v4Call(t, uniswapv4.SelectorGetSlot0, poolID),
		hexResult(word(q96()), signedWord(0), word(big.NewInt(protocolFee)), word(big.NewInt(lpFee))))
	caller.setOut(stateView, v4Call(t, uniswapv4.SelectorGetLiquidity, poolID), liquidityOut(1_000_000))
	for position := int64(-1); position <= 1; position++ {
		out := "0x"
		if position == 0 {
			out = hexResult(word(new(big.Int).SetBit(new(big.Int), 0, 1)))
		}
		caller.setOut(stateView, v4Call(t, uniswapv4.SelectorGetTickBitmap, poolID, signedWord(position)), out)
	}
	caller.setOut(stateView, v4Call(t, uniswapv4.SelectorGetTickLiquidity, poolID, signedWord(0)),
		hexResult(word(big.NewInt(700)), signedWord(700)))
}

func TestSeedV4StoresShallowAndDeep(t *testing.T) {
	// The configured LP fee is stale; StateView's slot0 value must win. It is
	// still part of the pool ID, so the ID commits to 3000.
	spec := withV4PoolID(t, withFee(v4Spec(t, 500), 3000))

	caller := newFakeCaller()
	setV4HappyPath(t, caller, spec.PoolID)
	stateView := stateViewAddress(t)

	store := state.New()
	ref := testBlockRef(0x1f)
	if err := Seed(context.Background(), caller, store, []Spec{spec}, ref, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	key := v4Key(spec)
	if key.Address != uniswapv4.PoolManager() || !key.IsV4() || key.Hooks != (domain.Address{}) {
		t.Fatalf("key = %+v, want the PoolManager address, V4 kind, and no hooks", key)
	}
	got, ok := store.Pool(key)
	if !ok {
		t.Fatalf("Pool(%s) missing after Seed", key.PoolID.Hex())
	}
	if got.Key != key {
		t.Fatalf("stored key = %+v, want %+v", got.Key, key)
	}
	if got.Fee != 500 || got.ProtocolFee != 0x07d07d {
		t.Fatalf("fees = %d/%#x, want 500/0x7d07d", got.Fee, got.ProtocolFee)
	}
	if got.TickSpacing != spec.TickSpacing {
		t.Fatalf("tick spacing = %d, want %d", got.TickSpacing, spec.TickSpacing)
	}
	if got.SqrtPriceX96.Cmp(q96()) != 0 || got.Tick != 0 {
		t.Fatalf("price/tick = (%s, %d), want (%s, 0)", got.SqrtPriceX96, got.Tick, q96())
	}
	if got.Liquidity.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("liquidity = %s, want 1000000", got.Liquidity)
	}

	deep, ok := store.Deep(key)
	if !ok {
		t.Fatalf("Deep(%s) missing after Seed", key.PoolID.Hex())
	}
	if len(deep.Bitmap) != 1 || deep.Bitmap[0] == nil || deep.Bitmap[0].Bit(0) != 1 {
		t.Fatalf("deep bitmap = %v, want word 0 bit 0", deep.Bitmap)
	}
	info, ok := deep.Ticks[0]
	if !ok || info.LiquidityGross.Cmp(big.NewInt(700)) != 0 || info.LiquidityNet.Cmp(big.NewInt(700)) != 0 {
		t.Fatalf("deep tick 0 = %+v, want gross 700 net 700", info)
	}
	if deep.LoadedBlock != ref {
		t.Fatalf("loaded block = %+v, want %+v", deep.LoadedBlock, ref)
	}
	if got := store.Block(); got != ref {
		t.Fatalf("store block = %+v, want %+v", got, ref)
	}

	if len(caller.calls) != 6 {
		t.Fatalf("calls = %d, want 6", len(caller.calls))
	}
	for _, call := range caller.calls {
		if call.to != stateView || call.block != "0x1f" {
			t.Fatalf("call = (%s, %s), want (%s, 0x1f)", call.to, call.block, stateView)
		}
	}
}

// TestSeedV4AcceptsNativeETHToken0 seeds a V4 pool whose currency0 is native
// ETH. address(0) is only valid as V4 token0; the stored key still carries the
// pool ID and hooks.
func TestSeedV4AcceptsNativeETHToken0(t *testing.T) {
	spec := withV4PoolID(t, withTokens(v4Spec(t, 500), domain.Address{}, testToken1))
	poolID := spec.PoolID

	caller := newFakeCaller()
	setV4HappyPath(t, caller, poolID)

	store := state.New()
	ref := testBlockRef(0x1f)
	if err := Seed(context.Background(), caller, store, []Spec{spec}, ref, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	key := v4Key(spec)
	got, ok := store.Pool(key)
	if !ok {
		t.Fatalf("Pool(%s) missing after Seed", poolID.Hex())
	}
	if got.Token0 != (domain.Address{}) {
		t.Fatalf("token0 = %s, want the zero address for native ETH", got.Token0)
	}
	if got.Token1 != testToken1 {
		t.Fatalf("token1 = %s, want %s", got.Token1, testToken1)
	}
	if got.Key.PoolID != poolID || got.Key.Hooks != (domain.Address{}) {
		t.Fatalf("key = %+v, want pool ID %s and no hooks", got.Key, poolID.Hex())
	}
	if _, ok := store.Deep(key); !ok {
		t.Fatalf("Deep(%s) missing after Seed", poolID.Hex())
	}
}

// TestSeedV4AcceptsZeroFee pins that zero is inside the V4 fee range: the
// configured fee only feeds the pool ID, and slot0 may report a zero LP fee.
func TestSeedV4AcceptsZeroFee(t *testing.T) {
	spec := v4Spec(t, 0)

	caller := newFakeCaller()
	setV4Path(t, caller, spec.PoolID, 0, 0)

	store := state.New()
	ref := testBlockRef(0x20)
	if err := Seed(context.Background(), caller, store, []Spec{spec}, ref, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	key := v4Key(spec)
	got, ok := store.Pool(key)
	if !ok {
		t.Fatalf("Pool(%s) missing after Seed", spec.PoolID.Hex())
	}
	if got.Fee != 0 || got.ProtocolFee != 0 {
		t.Fatalf("fees = %d/%#x, want 0/0", got.Fee, got.ProtocolFee)
	}

	// The pool ID commits to the fee, so an ID computed with a different fee
	// must be rejected even though the range check passes.
	otherID, err := uniswapv4.PoolID(spec.Token0, spec.Token1, 100, spec.TickSpacing, spec.Hooks)
	if err != nil {
		t.Fatalf("PoolID(100): %v", err)
	}
	wrong := withPoolID(spec, otherID)
	caller = newFakeCaller()
	err = Seed(context.Background(), caller, state.New(), []Spec{wrong}, ref, 1)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Seed with mismatched zero-fee pool ID = %v, want ErrInvalidSpec", err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("pool ID mismatch made %d contract calls", len(caller.calls))
	}
}
