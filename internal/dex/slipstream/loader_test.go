package slipstream

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
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

func validSpec() Spec {
	return Spec{
		Address:     domain.Address{0xbb},
		Token0:      domain.Address{0x01},
		Token1:      domain.Address{0x02},
		Fee:         500,
		TickSpacing: 10,
	}
}

func q96() *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), 96)
}

func selectorBytes(t *testing.T, selector string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(selector, "0x"))
	if err != nil {
		t.Fatalf("decode selector %s: %v", selector, err)
	}
	return raw
}

func word(value *big.Int) []byte {
	out := make([]byte, 32)
	value.FillBytes(out)
	return out
}

func signedWord(value int64) []byte {
	encoded := big.NewInt(value)
	if value < 0 {
		encoded.Add(encoded, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	out := make([]byte, 32)
	encoded.FillBytes(out)
	return out
}

func slot0Result(sqrtPriceX96 *big.Int, tick int64) string {
	data := append(word(sqrtPriceX96), signedWord(tick)...)
	return "0x" + hex.EncodeToString(data)
}

func liquidityResult(liquidity *big.Int) string {
	return "0x" + hex.EncodeToString(word(liquidity))
}

func feeResult(fee uint64) string {
	return "0x" + hex.EncodeToString(word(new(big.Int).SetUint64(fee)))
}

// liveFee is the fee() value setHappyPath answers with. It differs from
// validSpec's creation-time fee so a load that keeps the spec fee fails.
const liveFee uint64 = 3000

func setHappyPath(t *testing.T, f *fakeCaller, liquidity *big.Int) {
	t.Helper()
	f.setOut(selectorBytes(t, uniswapv3.SelectorSlot0), slot0Result(q96(), 0))
	f.setOut(selectorBytes(t, uniswapv3.SelectorLiquidity), liquidityResult(liquidity))
	bitmapSelector := selectorBytes(t, uniswapv3.SelectorTickBitmap)
	for position := int64(-1); position <= 1; position++ {
		f.setOut(append(bitmapSelector, signedWord(position)...), "0x")
	}
	f.setOut(selectorBytes(t, SelectorFee), feeResult(liveFee))
}

func TestLoad(t *testing.T) {
	spec := validSpec()
	fake := newFakeCaller()
	setHappyPath(t, fake, big.NewInt(1_000_000_000))

	state, deep, err := Load(context.Background(), fake, spec, "0x2a", 1)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Key.Address != spec.Address || state.Key.Kind != domain.PoolKindSlipstream {
		t.Fatalf("key = %+v, want address %s and Slipstream kind", state.Key, spec.Address)
	}
	if state.Token0 != spec.Token0 || state.Token1 != spec.Token1 {
		t.Fatalf("tokens = (%s, %s), want (%s, %s)", state.Token0, state.Token1, spec.Token0, spec.Token1)
	}
	// The dynamic fee() value wins over the creation-time spec fee.
	if state.Fee != uint32(liveFee) || state.TickSpacing != spec.TickSpacing {
		t.Fatalf("fee/spacing = (%d, %d), want (%d, %d)", state.Fee, state.TickSpacing, liveFee, spec.TickSpacing)
	}
	if state.SqrtPriceX96.Cmp(q96()) != 0 || state.Tick != 0 {
		t.Fatalf("price/tick = (%s, %d), want (%s, 0)", state.SqrtPriceX96, state.Tick, q96())
	}
	if state.Liquidity.Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Fatalf("liquidity = %s", state.Liquidity)
	}
	if deep.Bitmap == nil || deep.Ticks == nil {
		t.Fatal("deep maps must be initialized")
	}
	if len(deep.Bitmap) != 0 || len(deep.Ticks) != 0 {
		t.Fatalf("deep = %+v, want empty", deep)
	}

	if len(fake.calls) != 6 {
		t.Fatalf("calls = %d, want 6", len(fake.calls))
	}
	if got, want := hex.EncodeToString(fake.calls[0].data), uniswapv3.SelectorSlot0[2:]; got != want {
		t.Fatalf("first call data = %s, want slot0 selector %s", got, want)
	}
	if got, want := hex.EncodeToString(fake.calls[1].data), uniswapv3.SelectorLiquidity[2:]; got != want {
		t.Fatalf("second call data = %s, want liquidity selector %s", got, want)
	}
	if got, want := hex.EncodeToString(fake.calls[5].data), SelectorFee[2:]; got != want {
		t.Fatalf("last call data = %s, want fee selector %s", got, want)
	}
	for _, call := range fake.calls {
		if call.to != spec.Address || call.block != "0x2a" {
			t.Fatalf("call target = (%s, %s), want (%s, 0x2a)", call.to, call.block, spec.Address)
		}
	}
}

func TestLoadInvalidSpec(t *testing.T) {
	tests := []struct {
		name   string
		block  string
		caller func() pool.ContractCaller
		mutate func(*Spec)
	}{
		{name: "nil caller", block: "latest", caller: func() pool.ContractCaller { return nil }, mutate: func(*Spec) {}},
		{name: "zero address", block: "latest", mutate: func(s *Spec) { s.Address = domain.Address{} }},
		{name: "zero token0", block: "latest", mutate: func(s *Spec) { s.Token0 = domain.Address{} }},
		{name: "zero token1", block: "latest", mutate: func(s *Spec) { s.Token1 = domain.Address{} }},
		{name: "equal tokens", block: "latest", mutate: func(s *Spec) { s.Token1 = s.Token0 }},
		{name: "fee at denominator", block: "latest", mutate: func(s *Spec) { s.Fee = 1_000_000 }},
		{name: "zero tick spacing", block: "latest", mutate: func(s *Spec) { s.TickSpacing = 0 }},
		{name: "empty block", block: "", mutate: func(*Spec) {}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validSpec()
			test.mutate(&spec)
			var caller pool.ContractCaller = newFakeCaller()
			if test.caller != nil {
				caller = test.caller()
			}
			if _, _, err := Load(context.Background(), caller, spec, test.block, 1); !errors.Is(err, uniswapv3.ErrInvalidSpec) {
				t.Fatalf("error = %v, want uniswapv3.ErrInvalidSpec", err)
			}
		})
	}
}

func TestLoadPropagatesCallerError(t *testing.T) {
	fake := newFakeCaller()
	setHappyPath(t, fake, big.NewInt(1))
	errBoom := errors.New("boom")
	fake.setError(selectorBytes(t, uniswapv3.SelectorSlot0), errBoom)

	if _, _, err := Load(context.Background(), fake, validSpec(), "latest", 1); !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want boom", err)
	}
}

func TestLoadPropagatesFeeCallError(t *testing.T) {
	fake := newFakeCaller()
	setHappyPath(t, fake, big.NewInt(1))
	errBoom := errors.New("fee reverted")
	fake.setError(selectorBytes(t, SelectorFee), errBoom)

	_, _, err := Load(context.Background(), fake, validSpec(), "latest", 1)
	if !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want %v", err, errBoom)
	}
}

func TestLoadRejectsMalformedFee(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{name: "empty", out: ""},
		{name: "not hex", out: "0xzz"},
		{name: "short response", out: "0x01"},
		{name: "two words", out: "0x" + hex.EncodeToString(make([]byte, 64))},
		{name: "at denominator", out: feeResult(uint64(maxFeePips))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeCaller()
			setHappyPath(t, fake, big.NewInt(1))
			fake.setOut(selectorBytes(t, SelectorFee), test.out)

			if _, _, err := Load(context.Background(), fake, validSpec(), "latest", 1); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestFeeSelector(t *testing.T) {
	decoded := selectorBytes(t, SelectorFee)
	if len(decoded) != 4 {
		t.Fatalf("selector length = %d, want 4", len(decoded))
	}
	if got := hex.EncodeToString(feeCall); got != SelectorFee[2:] {
		t.Fatalf("feeCall = %s, want %s", got, SelectorFee[2:])
	}
}

func TestLoadRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Load(ctx, newFakeCaller(), validSpec(), "latest", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
