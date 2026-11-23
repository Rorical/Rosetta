package camelotv3

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
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

func validSpec() Spec {
	return Spec{
		Address:     domain.Address{0xaa},
		Token0:      domain.Address{0x01},
		Token1:      domain.Address{0x02},
		Fee:         3000,
		TickSpacing: 10,
	}
}

func q96() *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), 96)
}

func pow2(bits uint) *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), bits)
}

func word(value *big.Int) []byte {
	out := make([]byte, abiWordSize)
	value.FillBytes(out)
	return out
}

func signedWordData(value int64) []byte {
	encoded := big.NewInt(value)
	if value < 0 {
		encoded.Add(encoded, twoTo256)
	}
	out := make([]byte, abiWordSize)
	encoded.FillBytes(out)
	return out
}

func globalStateResult(price *big.Int, tick int64, fee *big.Int, extraWords int) string {
	data := append(word(price), signedWordData(tick)...)
	data = append(data, word(fee)...)
	data = append(data, make([]byte, abiWordSize*(3+extraWords))...)
	return "0x" + hex.EncodeToString(data)
}

func liquidityResult(liquidity *big.Int) string {
	return "0x" + hex.EncodeToString(word(liquidity))
}

func setHappyPath(f *fakeCaller, globalOut, liquidityOut string) {
	f.setOut(globalStateCall, globalOut)
	f.setOut(liquidityCall, liquidityOut)
}

func TestSelectorsDecode(t *testing.T) {
	if len(globalStateCall) != 4 {
		t.Fatalf("globalState selector length = %d, want 4", len(globalStateCall))
	}
	if len(liquidityCall) != 4 {
		t.Fatalf("liquidity selector length = %d, want 4", len(liquidityCall))
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name       string
		onChainFee uint32
		specFee    uint32
		tick       int64
		liquidity  *big.Int
		extraWords int
		wantFee    uint32
	}{
		{
			name:       "on-chain fee wins",
			onChainFee: 500,
			specFee:    3000,
			tick:       0,
			liquidity:  big.NewInt(1_000_000_000),
			wantFee:    500,
		},
		{
			name:       "zero on-chain fee falls back to spec",
			onChainFee: 0,
			specFee:    3000,
			tick:       0,
			liquidity:  big.NewInt(42),
			wantFee:    3000,
		},
		{
			name:       "negative tick",
			onChainFee: 100,
			specFee:    3000,
			tick:       -887272,
			liquidity:  big.NewInt(7),
			wantFee:    100,
		},
		{
			name:       "eight-word directional layout",
			onChainFee: 100,
			specFee:    3000,
			tick:       60,
			liquidity:  big.NewInt(9),
			extraWords: 2,
			wantFee:    100,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validSpec()
			spec.Fee = test.specFee
			fake := newFakeCaller()
			setHappyPath(fake,
				globalStateResult(q96(), test.tick, new(big.Int).SetUint64(uint64(test.onChainFee)), test.extraWords),
				liquidityResult(test.liquidity))

			state, err := Load(context.Background(), fake, spec, "0x1e")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if state.Key.Address != spec.Address || state.Key.Kind != domain.PoolKindCamelotV3 {
				t.Fatalf("key = %+v, want address %s and Camelot V3 kind", state.Key, spec.Address)
			}
			if state.Token0 != spec.Token0 || state.Token1 != spec.Token1 {
				t.Fatalf("tokens = (%s, %s), want (%s, %s)", state.Token0, state.Token1, spec.Token0, spec.Token1)
			}
			if state.Fee != test.wantFee {
				t.Fatalf("fee = %d, want %d", state.Fee, test.wantFee)
			}
			if state.TickSpacing != spec.TickSpacing {
				t.Fatalf("tick spacing = %d, want %d", state.TickSpacing, spec.TickSpacing)
			}
			if state.SqrtPriceX96.Cmp(q96()) != 0 {
				t.Fatalf("sqrt price = %s, want %s", state.SqrtPriceX96, q96())
			}
			if state.Tick != int32(test.tick) {
				t.Fatalf("tick = %d, want %d", state.Tick, test.tick)
			}
			if state.Liquidity.Cmp(test.liquidity) != 0 {
				t.Fatalf("liquidity = %s, want %s", state.Liquidity, test.liquidity)
			}
			if len(fake.calls) != 2 {
				t.Fatalf("calls = %d, want 2", len(fake.calls))
			}
			for _, call := range fake.calls {
				if call.to != spec.Address || call.block != "0x1e" {
					t.Fatalf("call target = (%s, %s), want (%s, 0x1e)", call.to, call.block, spec.Address)
				}
			}
		})
	}
}

func TestLoadMalformedResponses(t *testing.T) {
	goodGlobal := globalStateResult(q96(), 0, big.NewInt(100), 0)
	tests := []struct {
		name         string
		globalOut    string
		liquidityOut string
	}{
		{name: "empty globalState", globalOut: "", liquidityOut: liquidityResult(big.NewInt(1))},
		{name: "globalState not hex", globalOut: "0xzz", liquidityOut: liquidityResult(big.NewInt(1))},
		{name: "globalState short", globalOut: "0x" + hex.EncodeToString(make([]byte, abiWordSize*5)), liquidityOut: liquidityResult(big.NewInt(1))},
		{name: "price wider than uint160", globalOut: globalStateResult(pow2(160), 0, big.NewInt(100), 0), liquidityOut: liquidityResult(big.NewInt(1))},
		{name: "tick outside int24", globalOut: globalStateResult(q96(), 1<<23, big.NewInt(100), 0), liquidityOut: liquidityResult(big.NewInt(1))},
		{name: "fee wider than uint16", globalOut: globalStateResult(q96(), 0, pow2(16), 0), liquidityOut: liquidityResult(big.NewInt(1))},
		{name: "empty liquidity", globalOut: goodGlobal, liquidityOut: ""},
		{name: "liquidity wider than uint128", globalOut: goodGlobal, liquidityOut: liquidityResult(pow2(128))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeCaller()
			setHappyPath(fake, test.globalOut, test.liquidityOut)
			if _, err := Load(context.Background(), fake, validSpec(), "latest"); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadCallerErrors(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name  string
		stage []byte
	}{
		{name: "globalState", stage: globalStateCall},
		{name: "liquidity", stage: liquidityCall},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeCaller()
			setHappyPath(fake, globalStateResult(q96(), 0, big.NewInt(100), 0), liquidityResult(big.NewInt(1)))
			fake.setError(test.stage, errBoom)
			if _, err := Load(context.Background(), fake, validSpec(), "latest"); !errors.Is(err, errBoom) {
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
		{name: "nil caller", block: "latest", caller: func() pool.ContractCaller { return nil }, mutate: func(*Spec) {}},
		{name: "zero address", block: "latest", mutate: func(s *Spec) { s.Address = domain.Address{} }},
		{name: "zero token0", block: "latest", mutate: func(s *Spec) { s.Token0 = domain.Address{} }},
		{name: "zero token1", block: "latest", mutate: func(s *Spec) { s.Token1 = domain.Address{} }},
		{name: "equal tokens", block: "latest", mutate: func(s *Spec) { s.Token1 = s.Token0 }},
		{name: "fee at denominator", block: "latest", mutate: func(s *Spec) { s.Fee = maxFeePips }},
		{name: "zero tick spacing", block: "latest", mutate: func(s *Spec) { s.TickSpacing = 0 }},
		{name: "negative tick spacing", block: "latest", mutate: func(s *Spec) { s.TickSpacing = -10 }},
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
			if _, err := Load(context.Background(), caller, spec, test.block); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestLoadRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Load(ctx, newFakeCaller(), validSpec(), "latest"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
