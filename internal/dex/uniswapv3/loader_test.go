package uniswapv3

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type fakeResponse struct {
	out string
	err error
}

type recordedCall struct {
	to    domain.Address
	data  []byte
	block string
}

type fakeCaller struct {
	responses map[string]fakeResponse
	calls     []recordedCall
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{responses: make(map[string]fakeResponse)}
}

func (f *fakeCaller) setOut(data []byte, out string) {
	f.responses[hex.EncodeToString(data)] = fakeResponse{out: out}
}

func (f *fakeCaller) setError(data []byte, err error) {
	f.responses[hex.EncodeToString(data)] = fakeResponse{err: err}
}

func (f *fakeCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls = append(f.calls, recordedCall{
		to:    to,
		data:  append([]byte(nil), data...),
		block: block,
	})
	response, ok := f.responses[hex.EncodeToString(data)]
	if !ok {
		return fmt.Errorf("fake caller: unexpected call %x", data)
	}
	if response.err != nil {
		return response.err
	}
	*out = response.out
	return nil
}

func loaderSpec() Spec {
	return Spec{
		Address:     domain.Address{0xaa},
		Token0:      testToken0,
		Token1:      testToken1,
		Fee:         3000,
		TickSpacing: 10,
	}
}

func withSpec(mutate func(*Spec)) Spec {
	spec := loaderSpec()
	mutate(&spec)
	return spec
}

func testCallData(t *testing.T, selector, argument string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(selector, "0x") + argument)
	if err != nil {
		t.Fatalf("encode call data: %v", err)
	}
	return raw
}

func signedArgumentHex(value int64) string {
	encoded := big.NewInt(value)
	if value < 0 {
		encoded.Add(encoded, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return fmt.Sprintf("%064x", encoded)
}

func slot0Result(sqrtPriceX96, tick *big.Int) string {
	data := append(word(sqrtPriceX96), word(tick)...)
	data = append(data, make([]byte, abiWordSize*5)...)
	return "0x" + hex.EncodeToString(data)
}

func liquidityResult(liquidity *big.Int) string {
	return "0x" + hex.EncodeToString(word(liquidity))
}

func bitmapResult(bitmap *big.Int) string {
	return "0x" + hex.EncodeToString(word(bitmap))
}

func ticksResult(gross, net *big.Int) string {
	data := append(word(gross), word(net)...)
	data = append(data, make([]byte, abiWordSize*6)...)
	return "0x" + hex.EncodeToString(data)
}

func q96() *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), 96)
}

func setEmptyBitmap(t *testing.T, caller *fakeCaller, positions ...int16) {
	t.Helper()
	for _, position := range positions {
		caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(int64(position))), "0x")
	}
}

func validLoaderCaller(t *testing.T) *fakeCaller {
	t.Helper()
	caller := newFakeCaller()
	caller.setOut(testCallData(t, SelectorSlot0, ""), slot0Result(q96(), big.NewInt(0)))
	caller.setOut(testCallData(t, SelectorLiquidity, ""), liquidityResult(big.NewInt(1_000_000)))
	setEmptyBitmap(t, caller, -1, 0, 1)
	return caller
}

func TestLoadV3MalformedResults(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, caller *fakeCaller)
	}{
		{
			name: "slot0 bad hex",
			mutate: func(t *testing.T, caller *fakeCaller) {
				caller.setOut(testCallData(t, SelectorSlot0, ""), "0xnothex")
			},
		},
		{
			name: "slot0 too short",
			mutate: func(t *testing.T, caller *fakeCaller) {
				caller.setOut(testCallData(t, SelectorSlot0, ""), "0x"+hex.EncodeToString(word(q96())))
			},
		},
		{
			name: "slot0 sqrt price over uint160",
			mutate: func(t *testing.T, caller *fakeCaller) {
				overflow := new(big.Int).Lsh(big.NewInt(1), 160)
				caller.setOut(testCallData(t, SelectorSlot0, ""), slot0Result(overflow, big.NewInt(0)))
			},
		},
		{
			name: "slot0 tick over int24",
			mutate: func(t *testing.T, caller *fakeCaller) {
				caller.setOut(testCallData(t, SelectorSlot0, ""), slot0Result(q96(), big.NewInt(1<<23)))
			},
		},
		{
			name: "liquidity empty",
			mutate: func(t *testing.T, caller *fakeCaller) {
				caller.setOut(testCallData(t, SelectorLiquidity, ""), "")
			},
		},
		{
			name: "liquidity over uint128",
			mutate: func(t *testing.T, caller *fakeCaller) {
				overflow := new(big.Int).Lsh(big.NewInt(1), 128)
				caller.setOut(testCallData(t, SelectorLiquidity, ""), liquidityResult(overflow))
			},
		},
		{
			name: "bitmap bad hex",
			mutate: func(t *testing.T, caller *fakeCaller) {
				caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), "0xnothex")
			},
		},
		{
			name: "bitmap partial word",
			mutate: func(t *testing.T, caller *fakeCaller) {
				caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), "0x01")
			},
		},
		{
			name: "ticks too short",
			mutate: func(t *testing.T, caller *fakeCaller) {
				caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), bitmapResult(big.NewInt(2)))
				caller.setOut(testCallData(t, SelectorTicks, signedArgumentHex(10)), liquidityResult(big.NewInt(1)))
			},
		},
		{
			name: "ticks bad hex",
			mutate: func(t *testing.T, caller *fakeCaller) {
				caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), bitmapResult(big.NewInt(2)))
				caller.setOut(testCallData(t, SelectorTicks, signedArgumentHex(10)), "0xnothex")
			},
		},
		{
			name: "ticks gross over uint128",
			mutate: func(t *testing.T, caller *fakeCaller) {
				overflow := new(big.Int).Lsh(big.NewInt(1), 128)
				caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), bitmapResult(big.NewInt(2)))
				caller.setOut(testCallData(t, SelectorTicks, signedArgumentHex(10)), ticksResult(overflow, big.NewInt(0)))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := validLoaderCaller(t)
			test.mutate(t, caller)
			_, err := Load(context.Background(), caller, loaderSpec(), "0x40", 1)
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadV3Validation(t *testing.T) {
	caller := newFakeCaller()
	tests := []struct {
		name   string
		caller pool.ContractCaller
		spec   Spec
		block  string
		radius int16
	}{
		{"nil caller", nil, loaderSpec(), "0x1", 1},
		{"zero pool address", caller, withSpec(func(s *Spec) { s.Address = domain.Address{} }), "0x1", 1},
		{"zero token0", caller, withSpec(func(s *Spec) { s.Token0 = domain.Address{} }), "0x1", 1},
		{"zero token1", caller, withSpec(func(s *Spec) { s.Token1 = domain.Address{} }), "0x1", 1},
		{"equal tokens", caller, withSpec(func(s *Spec) { s.Token1 = s.Token0 }), "0x1", 1},
		{"fee at denominator", caller, withSpec(func(s *Spec) { s.Fee = maxFeePips }), "0x1", 1},
		{"zero tick spacing", caller, withSpec(func(s *Spec) { s.TickSpacing = 0 }), "0x1", 1},
		{"negative tick spacing", caller, withSpec(func(s *Spec) { s.TickSpacing = -10 }), "0x1", 1},
		{"empty block", caller, loaderSpec(), "", 1},
		{"zero radius", caller, loaderSpec(), "0x1", 0},
		{"radius too large", caller, loaderSpec(), "0x1", 65},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(context.Background(), test.caller, test.spec, test.block, test.radius)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestLoadV3CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	caller := newFakeCaller()

	_, err := Load(ctx, caller, loaderSpec(), "0x1", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("calls = %d, want none", len(caller.calls))
	}
}

func TestSelectorsDecode(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		decoded  []byte
	}{
		{"slot0", SelectorSlot0, slot0Call},
		{"liquidity", SelectorLiquidity, liquidityCall},
		{"tick bitmap", SelectorTickBitmap, tickBitmapCall},
		{"ticks", SelectorTicks, ticksCall},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.decoded) != 4 {
				t.Fatalf("decoded length = %d, want 4", len(test.decoded))
			}
			if got := "0x" + hex.EncodeToString(test.decoded); got != test.selector {
				t.Fatalf("decoded selector = %s, want %s", got, test.selector)
			}
		})
	}
}

func selectorOf(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	return "0x" + hex.EncodeToString(data[:4])
}

func callArguments(t *testing.T, calls []recordedCall, selector string) []int64 {
	t.Helper()
	var arguments []int64
	for _, call := range calls {
		if selectorOf(call.data) == selector {
			arguments = append(arguments, callArgument(t, call.data))
		}
	}
	return arguments
}

func assertCallArguments(t *testing.T, calls []recordedCall, selector string, want []int64) {
	t.Helper()
	got := callArguments(t, calls, selector)
	if len(got) != len(want) {
		t.Fatalf("%s calls = %v, want %v", selector, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s argument %d = %d, want %d", selector, i, got[i], want[i])
		}
	}
}

func callWithArgument(t *testing.T, calls []recordedCall, selector string, argument int64) recordedCall {
	t.Helper()
	for _, call := range calls {
		if selectorOf(call.data) == selector && callArgument(t, call.data) == argument {
			return call
		}
	}
	t.Fatalf("no %s call with argument %d", selector, argument)
	return recordedCall{}
}

func callArgument(t *testing.T, data []byte) int64 {
	t.Helper()
	if len(data) != 4+abiWordSize {
		t.Fatalf("call data length = %d, want %d", len(data), 4+abiWordSize)
	}
	value := new(big.Int).SetBytes(data[4:])
	if data[4]&0x80 != 0 {
		value.Sub(value, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	if !value.IsInt64() {
		t.Fatalf("call argument %s does not fit int64", value)
	}
	return value.Int64()
}

func TestLoadShallow(t *testing.T) {
	caller := newFakeCaller()
	sqrtPrice := big.NewInt(1_234_567_890)
	caller.setOut(testCallData(t, SelectorSlot0, ""), slot0Result(sqrtPrice, big.NewInt(-887272)))
	caller.setOut(testCallData(t, SelectorLiquidity, ""), liquidityResult(big.NewInt(42)))
	spec := loaderSpec()

	got, err := LoadShallow(context.Background(), caller, spec, "0x1234")
	if err != nil {
		t.Fatalf("LoadShallow: %v", err)
	}
	wantKey := domain.PoolKey{Address: spec.Address, Kind: domain.PoolKindV3}
	if got.Key != wantKey {
		t.Fatalf("key = %+v, want %+v", got.Key, wantKey)
	}
	if got.Token0 != spec.Token0 || got.Token1 != spec.Token1 {
		t.Fatalf("tokens = %s/%s, want %s/%s", got.Token0, got.Token1, spec.Token0, spec.Token1)
	}
	if got.Fee != spec.Fee || got.TickSpacing != spec.TickSpacing {
		t.Fatalf("fee/spacing = %d/%d, want %d/%d", got.Fee, got.TickSpacing, spec.Fee, spec.TickSpacing)
	}
	if got.SqrtPriceX96.Cmp(sqrtPrice) != 0 {
		t.Fatalf("sqrt price = %s, want %s", got.SqrtPriceX96, sqrtPrice)
	}
	if got.Tick != -887272 {
		t.Fatalf("tick = %d, want -887272", got.Tick)
	}
	if got.Liquidity.Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("liquidity = %s, want 42", got.Liquidity)
	}

	if len(caller.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(caller.calls))
	}
	wantCalls := [][]byte{
		testCallData(t, SelectorSlot0, ""),
		testCallData(t, SelectorLiquidity, ""),
	}
	for i, call := range caller.calls {
		if call.to != spec.Address || call.block != "0x1234" {
			t.Fatalf("call %d = %s at %s, want %s at 0x1234", i, call.to.Hex(), call.block, spec.Address.Hex())
		}
		if !bytes.Equal(call.data, wantCalls[i]) {
			t.Fatalf("call %d data = %x, want %x", i, call.data, wantCalls[i])
		}
		for _, forbidden := range [][]byte{tickBitmapCall, ticksCall} {
			if bytes.HasPrefix(call.data, forbidden) {
				t.Fatalf("call %d read deep tick storage", i)
			}
		}
	}
}

func TestLoadShallowRejectsBadSpec(t *testing.T) {
	caller := validLoaderCaller(t)
	if _, err := LoadShallow(context.Background(), nil, loaderSpec(), "0x1"); err == nil {
		t.Fatal("nil caller accepted")
	}
	sameTokens := withSpec(func(spec *Spec) { spec.Token1 = spec.Token0 })
	if _, err := LoadShallow(context.Background(), caller, sameTokens, "0x1"); err == nil {
		t.Fatal("identical tokens accepted")
	}
	if len(caller.calls) != 0 {
		t.Fatalf("calls = %d before rejection, want 0", len(caller.calls))
	}
}
