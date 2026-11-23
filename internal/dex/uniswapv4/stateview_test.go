package uniswapv4

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type stateKey struct {
	to   domain.Address
	data string
}

type stateResponse struct {
	out string
	err error
}

type stateCall struct {
	to    domain.Address
	data  []byte
	block string
}

type stateCaller struct {
	responses map[stateKey]stateResponse
	calls     []stateCall
}

func newStateCaller() *stateCaller {
	return &stateCaller{responses: make(map[stateKey]stateResponse)}
}

func (f *stateCaller) setOut(to domain.Address, data []byte, out string) {
	f.responses[stateKey{to: to, data: hex.EncodeToString(data)}] = stateResponse{out: out}
}

func (f *stateCaller) setError(to domain.Address, data []byte, err error) {
	f.responses[stateKey{to: to, data: hex.EncodeToString(data)}] = stateResponse{err: err}
}

func (f *stateCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls = append(f.calls, stateCall{to: to, data: append([]byte(nil), data...), block: block})
	response, ok := f.responses[stateKey{to: to, data: hex.EncodeToString(data)}]
	if !ok {
		return errors.New("state caller: unexpected call")
	}
	if response.err != nil {
		return response.err
	}
	*out = response.out
	return nil
}

func stateViewData(selector string, poolID domain.Hash) []byte {
	return append(decodeSelector(selector), poolID[:]...)
}

func slot0Result(sqrtPriceX96 *big.Int, tick, protocolFee, lpFee int64) string {
	return "0x" + hex.EncodeToString(slot0Words(sqrtPriceX96, tick, protocolFee, lpFee))
}

func slot0Words(sqrtPriceX96 *big.Int, tick, protocolFee, lpFee int64) []byte {
	data := append(word(sqrtPriceX96), signedWordData(tick)...)
	data = append(data, word(big.NewInt(protocolFee))...)
	data = append(data, word(big.NewInt(lpFee))...)
	return data
}

func liquidityResult(value *big.Int) string {
	return "0x" + hex.EncodeToString(word(value))
}

func validSpec() Spec {
	return Spec{
		PoolID: testPoolID,
	}
}

func TestStateViewSelectorsAndAddresses(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		decoded  []byte
	}{
		{"getSlot0", SelectorGetSlot0, getSlot0Call},
		{"getLiquidity", SelectorGetLiquidity, getLiquidityCall},
		{"getTickBitmap", SelectorGetTickBitmap, getTickBitmapCall},
		{"getTickLiquidity", SelectorGetTickLiquidity, getTickLiquidityCall},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.decoded) != 4 {
				t.Fatalf("%s length = %d, want 4", test.name, len(test.decoded))
			}
			if got := "0x" + hex.EncodeToString(test.decoded); got != test.selector {
				t.Fatalf("decoded selector = %s, want %s", got, test.selector)
			}
		})
	}
	if got := stateViewAddress.Hex(); got != StateViewAddress {
		t.Fatalf("stateViewAddress = %s, want %s", got, StateViewAddress)
	}
	if got := poolManagerAddress.Hex(); got != PoolManagerAddress {
		t.Fatalf("poolManagerAddress = %s, want %s", got, PoolManagerAddress)
	}
}

func TestLoadState(t *testing.T) {
	tests := []struct {
		name        string
		sqrtPrice   *big.Int
		tick        int64
		protocolFee int64
		lpFee       int64
		liquidity   *big.Int
	}{
		{name: "initialized", sqrtPrice: q96(), tick: -123, lpFee: 500, liquidity: big.NewInt(1_000_000)},
		{name: "protocol fee is kept beside lpFee", sqrtPrice: q96(), tick: 60, protocolFee: 0x07d07d, lpFee: 3_000, liquidity: big.NewInt(7)},
		{name: "uninitialized pool", sqrtPrice: big.NewInt(0), tick: 0, liquidity: big.NewInt(0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newStateCaller()
			caller.setOut(stateViewAddress, stateViewData(SelectorGetSlot0, testPoolID),
				slot0Result(test.sqrtPrice, test.tick, test.protocolFee, test.lpFee))
			caller.setOut(stateViewAddress, stateViewData(SelectorGetLiquidity, testPoolID),
				liquidityResult(test.liquidity))

			state, err := LoadState(context.Background(), caller, testPoolID, "0x10")
			if err != nil {
				t.Fatalf("LoadState: %v", err)
			}
			wantKey := domain.PoolKey{Address: poolManagerAddress, Kind: domain.PoolKindV4, PoolID: testPoolID}
			if state.Key != wantKey {
				t.Fatalf("key = %+v, want %+v", state.Key, wantKey)
			}
			if state.Token0 != (domain.Address{}) || state.Token1 != (domain.Address{}) {
				t.Fatalf("tokens = %s/%s, want zero without a spec", state.Token0, state.Token1)
			}
			if state.TickSpacing != 0 {
				t.Fatalf("tick spacing = %d, want 0 without a spec", state.TickSpacing)
			}
			if state.Fee != uint32(test.lpFee) {
				t.Fatalf("fee = %d, want lpFee %d", state.Fee, test.lpFee)
			}
			if state.ProtocolFee != uint32(test.protocolFee) {
				t.Fatalf("protocol fee = %d, want slot0 protocolFee %d", state.ProtocolFee, test.protocolFee)
			}
			if state.SqrtPriceX96.Cmp(test.sqrtPrice) != 0 {
				t.Fatalf("sqrt price = %s, want %s", state.SqrtPriceX96, test.sqrtPrice)
			}
			if state.Tick != int32(test.tick) {
				t.Fatalf("tick = %d, want %d", state.Tick, test.tick)
			}
			if state.Liquidity.Cmp(test.liquidity) != 0 {
				t.Fatalf("liquidity = %s, want %s", state.Liquidity, test.liquidity)
			}
			if len(caller.calls) != 2 {
				t.Fatalf("calls = %d, want 2", len(caller.calls))
			}
			if caller.calls[0].to != stateViewAddress || caller.calls[1].to != stateViewAddress {
				t.Fatalf("calls did not target StateView")
			}
			if caller.calls[0].block != "0x10" || caller.calls[1].block != "0x10" {
				t.Fatalf("call blocks = %q/%q, want 0x10", caller.calls[0].block, caller.calls[1].block)
			}
		})
	}
}

func TestLoadWithSpecFillsIdentity(t *testing.T) {
	caller := newStateCaller()
	caller.setOut(stateViewAddress, stateViewData(SelectorGetSlot0, testPoolID), slot0Result(q96(), 30, 0x07d07d, 100))
	caller.setOut(stateViewAddress, stateViewData(SelectorGetLiquidity, testPoolID), liquidityResult(big.NewInt(42)))

	spec := Spec{
		PoolID:      testPoolID,
		Token0:      domain.Address{0x01},
		Token1:      domain.Address{0x02},
		TickSpacing: 60,
		Hooks:       domain.Address{0x03},
	}
	state, err := Load(context.Background(), caller, spec, "latest")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Token0 != spec.Token0 || state.Token1 != spec.Token1 || state.TickSpacing != spec.TickSpacing {
		t.Fatalf("identity = (%s, %s, %d), want (%s, %s, %d)",
			state.Token0, state.Token1, state.TickSpacing, spec.Token0, spec.Token1, spec.TickSpacing)
	}
	if state.Key.PoolID != spec.PoolID || state.Key.Hooks != spec.Hooks {
		t.Fatalf("key = %+v, want pool ID %s and hooks %s", state.Key, spec.PoolID.Hex(), spec.Hooks)
	}
	if state.Fee != 100 || state.ProtocolFee != 0x07d07d || state.Tick != 30 {
		t.Fatalf("fee/protocol fee/tick = %d/%#x/%d, want 100/0x7d07d/30",
			state.Fee, state.ProtocolFee, state.Tick)
	}

	if got, want := hex.EncodeToString(caller.calls[0].data), hex.EncodeToString(stateViewData(SelectorGetSlot0, testPoolID)); got != want {
		t.Fatalf("getSlot0 data = %s, want %s", got, want)
	}
	if got, want := hex.EncodeToString(caller.calls[1].data), hex.EncodeToString(stateViewData(SelectorGetLiquidity, testPoolID)); got != want {
		t.Fatalf("getLiquidity data = %s, want %s", got, want)
	}
}

func TestParseSlot0ReadsBothFees(t *testing.T) {
	slot, err := parseSlot0(slot0Result(q96(), -7, 1_000, 500))
	if err != nil {
		t.Fatalf("parseSlot0: %v", err)
	}
	if slot.protocolFee != 1_000 || slot.lpFee != 500 {
		t.Fatalf("fees = %d/%d, want 1000/500", slot.protocolFee, slot.lpFee)
	}
	if slot.tick != -7 {
		t.Fatalf("tick = %d, want -7", slot.tick)
	}
}

func TestLoadMalformedResponses(t *testing.T) {
	tests := []struct {
		name      string
		slot0     string
		liquidity string
	}{
		{name: "empty slot0", slot0: "", liquidity: liquidityResult(big.NewInt(1))},
		{name: "slot0 not hex", slot0: "0xzz", liquidity: liquidityResult(big.NewInt(1))},
		{name: "slot0 short", slot0: "0x" + strings.Repeat("00", abiWordSize*3), liquidity: liquidityResult(big.NewInt(1))},
		{name: "price wider than uint160", slot0: slot0Result(pow2(160), 0, 0, 0), liquidity: liquidityResult(big.NewInt(1))},
		{name: "tick outside int24", slot0: slot0Result(q96(), 1<<23, 0, 0), liquidity: liquidityResult(big.NewInt(1))},
		{name: "protocol fee wider than uint24", slot0: slot0Result(q96(), 0, 1<<24, 0), liquidity: liquidityResult(big.NewInt(1))},
		{name: "lp fee wider than uint24", slot0: slot0Result(q96(), 0, 0, 1<<24), liquidity: liquidityResult(big.NewInt(1))},
		{name: "empty liquidity", slot0: slot0Result(q96(), 0, 0, 500), liquidity: ""},
		{name: "liquidity wider than uint128", slot0: slot0Result(q96(), 0, 0, 500), liquidity: liquidityResult(pow2(128))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newStateCaller()
			caller.setOut(stateViewAddress, stateViewData(SelectorGetSlot0, testPoolID), test.slot0)
			caller.setOut(stateViewAddress, stateViewData(SelectorGetLiquidity, testPoolID), test.liquidity)

			if _, err := LoadState(context.Background(), caller, testPoolID, "latest"); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadCallerErrors(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name     string
		selector string
	}{
		{name: "getSlot0", selector: SelectorGetSlot0},
		{name: "getLiquidity", selector: SelectorGetLiquidity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newStateCaller()
			caller.setOut(stateViewAddress, stateViewData(SelectorGetSlot0, testPoolID), slot0Result(q96(), 0, 0, 500))
			caller.setOut(stateViewAddress, stateViewData(SelectorGetLiquidity, testPoolID), liquidityResult(big.NewInt(1)))
			caller.setError(stateViewAddress, stateViewData(test.selector, testPoolID), errBoom)

			if _, err := LoadState(context.Background(), caller, testPoolID, "latest"); !errors.Is(err, errBoom) {
				t.Fatalf("error = %v, want boom", err)
			}
		})
	}
}

func TestLoadInvalidSpec(t *testing.T) {
	tests := []struct {
		name   string
		block  string
		caller func() pool.ContractCaller
		mutate func(*Spec)
	}{
		{name: "nil caller", block: "latest", caller: func() pool.ContractCaller { return nil }},
		{name: "zero pool ID", block: "latest", mutate: func(s *Spec) { s.PoolID = domain.Hash{} }},
		{name: "empty block", block: "", mutate: func(*Spec) {}},
		{name: "negative tick spacing", block: "latest", mutate: func(s *Spec) { s.TickSpacing = -1 }},
		{name: "only token0", block: "latest", mutate: func(s *Spec) { s.Token0 = domain.Address{0x01} }},
		{name: "equal tokens", block: "latest", mutate: func(s *Spec) {
			s.Token0 = domain.Address{0x01}
			s.Token1 = domain.Address{0x01}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validSpec()
			if test.mutate != nil {
				test.mutate(&spec)
			}
			var caller pool.ContractCaller = newStateCaller()
			if test.caller != nil {
				caller = test.caller()
			}
			if _, err := Load(context.Background(), caller, spec, test.block); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

// TestLoadAcceptsNativeETHToken0 loads a spec whose currency0 is address(0),
// the representation V4 uses for native ETH.
func TestLoadAcceptsNativeETHToken0(t *testing.T) {
	caller := newStateCaller()
	caller.setOut(stateViewAddress, stateViewData(SelectorGetSlot0, testPoolID), slot0Result(q96(), 0, 0, 500))
	caller.setOut(stateViewAddress, stateViewData(SelectorGetLiquidity, testPoolID), liquidityResult(big.NewInt(1)))
	spec := Spec{PoolID: testPoolID, Token1: domain.Address{0x02}, TickSpacing: 10}

	state, err := Load(context.Background(), caller, spec, "0x10")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Token0 != (domain.Address{}) {
		t.Fatalf("token0 = %s, want the zero address", state.Token0)
	}
	if state.Token1 != spec.Token1 {
		t.Fatalf("token1 = %s, want %s", state.Token1, spec.Token1)
	}
}

func TestLoadRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadState(ctx, newStateCaller(), testPoolID, "latest"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestLoaderIsNotAnAdapter fails if Loader grows the pool.Adapter method set.
// State reads and quotes are separate types; only Adapter may implement
// pool.Adapter. Go has no negative interface assertion, so this reflection
// check in the test build is the closest available guard.
func TestLoaderIsNotAnAdapter(t *testing.T) {
	adapter := reflect.TypeOf((*pool.Adapter)(nil)).Elem()
	for _, value := range []any{Loader{}, (*Loader)(nil)} {
		typ := reflect.TypeOf(value)
		if typ.Implements(adapter) {
			t.Fatalf("%s implements pool.Adapter; quoting belongs to Adapter", typ)
		}
		if _, ok := typ.MethodByName("QuoteExactIn"); ok {
			t.Fatalf("%s has QuoteExactIn; quoting belongs to Adapter", typ)
		}
	}
}

type networkStateCaller struct {
	*stateCaller
	manager, view domain.Address
}

func (c networkStateCaller) V4Deployment() (domain.Address, domain.Address) { return c.manager, c.view }

func TestStateViewUsesCallerDeployment(t *testing.T) {
	caller := networkStateCaller{stateCaller: newStateCaller(), manager: domain.Address{1}, view: domain.Address{2}}
	caller.setOut(caller.view, stateViewData(SelectorGetSlot0, testPoolID), slot0Result(q96(), 30, 0, 100))
	caller.setOut(caller.view, stateViewData(SelectorGetLiquidity, testPoolID), liquidityResult(big.NewInt(42)))
	loaded, err := LoadState(t.Context(), caller, testPoolID, "0x1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Key.Address != caller.manager || len(caller.calls) != 2 || caller.calls[0].to != caller.view {
		t.Fatal("loader used another chain's deployment")
	}
	caller.manager, caller.view = domain.Address{}, domain.Address{}
	if _, err := LoadState(t.Context(), caller, testPoolID, "0x1"); err == nil {
		t.Fatal("disabled V4 deployment fell back to One")
	}
}
