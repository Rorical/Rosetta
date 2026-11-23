package uniswapv4

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

type deepCallKey struct {
	to   domain.Address
	data string
}

type deepResponse struct {
	out string
	err error
}

// deepFakeCaller answers StateView calls and records them. It implements
// pool.BatchContractCaller, so tests that need the sequential path wrap it in
// sequentialCaller.
type deepFakeCaller struct {
	responses   map[deepCallKey]deepResponse
	calls       []stateCall
	batchRounds int
}

func newDeepCaller() *deepFakeCaller {
	return &deepFakeCaller{responses: make(map[deepCallKey]deepResponse)}
}

func (f *deepFakeCaller) set(selector string, poolID domain.Hash, argument []byte, out string) {
	data := stateViewCallData(decodeSelector(selector), poolID, argument)
	f.responses[deepCallKey{to: stateViewAddress, data: hex.EncodeToString(data)}] = deepResponse{out: out}
}

func (f *deepFakeCaller) setError(selector string, poolID domain.Hash, argument []byte, err error) {
	data := stateViewCallData(decodeSelector(selector), poolID, argument)
	f.responses[deepCallKey{to: stateViewAddress, data: hex.EncodeToString(data)}] = deepResponse{err: err}
}

func (f *deepFakeCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls = append(f.calls, stateCall{to: to, data: append([]byte(nil), data...), block: block})
	response, ok := f.responses[deepCallKey{to: to, data: hex.EncodeToString(data)}]
	if !ok {
		return errors.New("deep caller: unexpected call")
	}
	if response.err != nil {
		return response.err
	}
	*out = response.out
	return nil
}

func (f *deepFakeCaller) CallContracts(ctx context.Context, calls []pool.ContractCall) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.batchRounds++
	results := make([]string, len(calls))
	for i, call := range calls {
		if err := f.CallContract(ctx, call.To, call.Data, call.Block, &results[i]); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// sequentialCaller hides the batch interface, forcing the one-call path.
type sequentialCaller struct {
	fake *deepFakeCaller
}

func (s sequentialCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	return s.fake.CallContract(ctx, to, data, block, out)
}

func deepTestState() pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: poolManagerAddress,
			Kind:    domain.PoolKindV4,
			PoolID:  testPoolID,
		},
		Token0:       domain.Address{0x01},
		Token1:       domain.Address{0x02},
		Fee:          500,
		ProtocolFee:  0x07d07d,
		TickSpacing:  10,
		SqrtPriceX96: q96(),
		Tick:         -100,
		Liquidity:    big.NewInt(1_000_000),
	}
}

func bitmapResult(word *big.Int) string {
	return "0x" + hex.EncodeToString(wordData(word))
}

func tickLiquidityResult(gross, net *big.Int) string {
	return "0x" + hex.EncodeToString(append(wordData(gross), signedWordDataBig(net)...))
}

func wordData(value *big.Int) []byte {
	out := make([]byte, abiWordSize)
	value.FillBytes(out)
	return out
}

func signedWordDataBig(value *big.Int) []byte {
	encoded := new(big.Int).Set(value)
	if encoded.Sign() < 0 {
		encoded.Add(encoded, twoTo256)
	}
	out := make([]byte, abiWordSize)
	encoded.FillBytes(out)
	return out
}
func TestBitmapPositions(t *testing.T) {
	tests := []struct {
		name        string
		tick        int32
		tickSpacing int32
		radius      int16
		want        []int16
		wantErr     bool
	}{
		{name: "aligned positive", tick: 2560, tickSpacing: 10, radius: 1, want: []int16{0, 1, 2}},
		{name: "negative tick floors", tick: -101, tickSpacing: 10, radius: 1, want: []int16{-2, -1, 0}},
		{name: "minimum tick", tick: math.MinTick, tickSpacing: 1, radius: 1, want: []int16{-3467, -3466, -3465}},
		{name: "int16 overflow", tick: 1 << 30, tickSpacing: 1, radius: 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := bitmapPositions(test.tick, test.tickSpacing, test.radius)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidResponse) {
					t.Fatalf("error = %v, want ErrInvalidResponse", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("bitmapPositions: %v", err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("positions = %v, want %v", got, test.want)
			}
		})
	}
}

func TestEncodeSignedWord(t *testing.T) {
	tests := []struct {
		name  string
		value int64
		want  string
	}{
		{"zero", 0, strings.Repeat("00", 32)},
		{"one", 1, strings.Repeat("00", 31) + "01"},
		{"minus one", -1, strings.Repeat("ff", 32)},
		{"minus ten", -10, strings.Repeat("ff", 31) + "f6"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hex.EncodeToString(encodeSignedWord(test.value)); got != test.want {
				t.Fatalf("encoding = %s, want %s", got, test.want)
			}
		})
	}
}

func assertDeepTick(t *testing.T, deep pool.DeepState, tick int32, gross, net int64) {
	t.Helper()
	info, ok := deep.Ticks[tick]
	if !ok {
		t.Fatalf("tick %d missing", tick)
	}
	if info.LiquidityGross.Int64() != gross || info.LiquidityNet.Int64() != net {
		t.Fatalf("tick %d = (%s, %s), want (%d, %d)",
			tick, info.LiquidityGross, info.LiquidityNet, gross, net)
	}
}

func assertDeepStateEqual(t *testing.T, got, want pool.DeepState) {
	t.Helper()
	if got.LoadedBlock != want.LoadedBlock {
		t.Fatalf("loaded block = %+v, want %+v", got.LoadedBlock, want.LoadedBlock)
	}
	if got.WindowMin != want.WindowMin || got.WindowMax != want.WindowMax {
		t.Fatalf("window = [%d, %d], want [%d, %d]", got.WindowMin, got.WindowMax, want.WindowMin, want.WindowMax)
	}
	if len(got.Bitmap) != len(want.Bitmap) || len(got.Ticks) != len(want.Ticks) {
		t.Fatalf("deep sizes = %d bitmap/%d ticks, want %d/%d",
			len(got.Bitmap), len(got.Ticks), len(want.Bitmap), len(want.Ticks))
	}
	for position, word := range want.Bitmap {
		if got.Bitmap[position].Cmp(word) != 0 {
			t.Fatalf("bitmap[%d] = %v, want %s", position, got.Bitmap[position], word)
		}
	}
	for tick, info := range want.Ticks {
		gotInfo := got.Ticks[tick]
		if gotInfo.LiquidityGross.Cmp(info.LiquidityGross) != 0 || gotInfo.LiquidityNet.Cmp(info.LiquidityNet) != 0 {
			t.Fatalf("tick %d = (%v, %v), want (%s, %s)",
				tick, gotInfo.LiquidityGross, gotInfo.LiquidityNet, info.LiquidityGross, info.LiquidityNet)
		}
	}
}
