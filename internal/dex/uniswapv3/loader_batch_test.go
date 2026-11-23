package uniswapv3

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

type fakeBatchCaller struct {
	responses   map[string]fakeResponse
	batches     [][]pool.ContractCall
	dropResults bool
}

var (
	_ pool.ContractCaller      = (*fakeBatchCaller)(nil)
	_ pool.BatchContractCaller = (*fakeBatchCaller)(nil)
)

// CallContract makes the fake usable with Load, whose argument still requires
// pool.ContractCaller. The batch method is the path under test.
func (f *fakeBatchCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	response, ok := f.responses[hex.EncodeToString(data)]
	if !ok {
		return fmt.Errorf("fake batch caller: unexpected call %x", data)
	}
	if response.err != nil {
		return response.err
	}
	*out = response.out
	return nil
}

func newFakeBatchCaller() *fakeBatchCaller {
	return &fakeBatchCaller{responses: make(map[string]fakeResponse)}
}

func (f *fakeBatchCaller) setOut(data []byte, out string) {
	f.responses[hex.EncodeToString(data)] = fakeResponse{out: out}
}

func (f *fakeBatchCaller) setError(data []byte, err error) {
	f.responses[hex.EncodeToString(data)] = fakeResponse{err: err}
}

func (f *fakeBatchCaller) CallContracts(ctx context.Context, calls []pool.ContractCall) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	recorded := make([]pool.ContractCall, len(calls))
	for i, call := range calls {
		recorded[i] = pool.ContractCall{
			To:    call.To,
			Data:  append([]byte(nil), call.Data...),
			Block: call.Block,
		}
	}
	f.batches = append(f.batches, recorded)

	out := make([]string, len(calls))
	for i, call := range calls {
		response, ok := f.responses[hex.EncodeToString(call.Data)]
		if !ok {
			return nil, fmt.Errorf("fake batch caller: unexpected call %x", call.Data)
		}
		if response.err != nil {
			return nil, response.err
		}
		out[i] = response.out
	}
	if f.dropResults {
		return out[:len(out)-1], nil
	}
	return out, nil
}

type responseSetter interface {
	setOut(data []byte, out string)
	setError(data []byte, err error)
}

// setDeepResponses serves the fixture used by both loaders: tick 30, two set
// bits in bitmap word 0, and one tick read per set bit.
func setDeepResponses(t *testing.T, setter responseSetter) {
	t.Helper()
	setter.setOut(testCallData(t, SelectorSlot0, ""), slot0Result(q96(), big.NewInt(30)))
	setter.setOut(testCallData(t, SelectorLiquidity, ""), liquidityResult(big.NewInt(1_500_000)))

	bitmap := new(big.Int).SetBit(new(big.Int), 5, 1) // tick 50
	bitmap.SetBit(bitmap, 12, 1)                      // tick 120
	for _, position := range []int16{-2, -1, 0, 1, 2} {
		out := "0x"
		if position == 0 {
			out = bitmapResult(bitmap)
		}
		setter.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(int64(position))), out)
	}
	setter.setOut(testCallData(t, SelectorTicks, signedArgumentHex(50)), ticksResult(big.NewInt(700), big.NewInt(700)))
	setter.setOut(testCallData(t, SelectorTicks, signedArgumentHex(120)), ticksResult(big.NewInt(300), big.NewInt(-300)))
}

func TestLoadV3BatchedMatchesSequential(t *testing.T) {
	sequential := newFakeCaller()
	setDeepResponses(t, sequential)
	batched := newFakeBatchCaller()
	setDeepResponses(t, batched)

	want, err := Load(context.Background(), sequential, loaderSpec(), "0x10", 2)
	if err != nil {
		t.Fatalf("sequential Load: %v", err)
	}
	got, err := Load(context.Background(), batched, loaderSpec(), "0x10", 2)
	if err != nil {
		t.Fatalf("batched Load: %v", err)
	}
	assertLoadedEqual(t, got, want)
}

func TestLoadV3BatchedCallLayout(t *testing.T) {
	spec := loaderSpec()
	caller := newFakeBatchCaller()
	setDeepResponses(t, caller)

	if _, err := Load(context.Background(), caller, spec, "0x10", 2); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(caller.batches) != 3 {
		t.Fatalf("batches = %d, want 3", len(caller.batches))
	}

	shallow := caller.batches[0]
	if len(shallow) != 2 {
		t.Fatalf("shallow batch calls = %d, want 2", len(shallow))
	}
	if got := selectorOf(shallow[0].Data); got != SelectorSlot0 {
		t.Fatalf("shallow call 0 selector = %s, want slot0", got)
	}
	if got := selectorOf(shallow[1].Data); got != SelectorLiquidity {
		t.Fatalf("shallow call 1 selector = %s, want liquidity", got)
	}

	bitmap := caller.batches[1]
	assertCallArguments(t, recordedCalls(bitmap), SelectorTickBitmap, []int64{-2, -1, 0, 1, 2})
	for i := range bitmap {
		if got := selectorOf(bitmap[i].Data); got != SelectorTickBitmap {
			t.Fatalf("bitmap call %d selector = %s", i, got)
		}
	}

	ticks := caller.batches[2]
	assertCallArguments(t, recordedCalls(ticks), SelectorTicks, []int64{50, 120})
	seen := make(map[int64]struct{}, len(ticks))
	for i := range ticks {
		if got := selectorOf(ticks[i].Data); got != SelectorTicks {
			t.Fatalf("ticks call %d selector = %s", i, got)
		}
		argument := callArgument(t, ticks[i].Data)
		if _, ok := seen[argument]; ok {
			t.Fatalf("duplicate ticks(%d) call", argument)
		}
		seen[argument] = struct{}{}
	}

	for batchIndex, batch := range caller.batches {
		for callIndex, call := range batch {
			if call.To != spec.Address {
				t.Fatalf("batch %d call %d to %s, want %s", batchIndex, callIndex, call.To, spec.Address)
			}
			if call.Block != "0x10" {
				t.Fatalf("batch %d call %d block = %q, want 0x10", batchIndex, callIndex, call.Block)
			}
		}
	}
}

func TestLoadV3BatchedEmptyBitmapSkipsTicksBatch(t *testing.T) {
	caller := newFakeBatchCaller()
	caller.setOut(testCallData(t, SelectorSlot0, ""), slot0Result(q96(), big.NewInt(0)))
	caller.setOut(testCallData(t, SelectorLiquidity, ""), liquidityResult(big.NewInt(1_000)))
	setEmptyBitmapBatch(t, caller, -1, 0, 1)

	loaded, err := Load(context.Background(), caller, loaderSpec(), "0x20", 1)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Deep.Bitmap) != 0 || len(loaded.Deep.Ticks) != 0 {
		t.Fatalf("deep state = %d bitmap, %d ticks, want empty", len(loaded.Deep.Bitmap), len(loaded.Deep.Ticks))
	}
	if len(caller.batches) != 2 {
		t.Fatalf("batches = %d, want 2", len(caller.batches))
	}
}

func TestLoadV3BatchedErrorPropagates(t *testing.T) {
	errBatch := errors.New("batch unavailable")
	tests := []struct {
		name   string
		mutate func(*fakeBatchCaller)
	}{
		{"shallow batch", func(c *fakeBatchCaller) {
			c.setError(testCallData(t, SelectorSlot0, ""), errBatch)
		}},
		{"bitmap batch", func(c *fakeBatchCaller) {
			c.setError(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), errBatch)
		}},
		{"ticks batch", func(c *fakeBatchCaller) {
			c.setError(testCallData(t, SelectorTicks, signedArgumentHex(50)), errBatch)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeBatchCaller()
			setDeepResponses(t, caller)
			test.mutate(caller)

			_, err := Load(context.Background(), caller, loaderSpec(), "0x10", 2)
			if !errors.Is(err, errBatch) {
				t.Fatalf("error = %v, want %v", err, errBatch)
			}
		})
	}
}

func TestLoadV3BatchedMalformedResults(t *testing.T) {
	overflow := new(big.Int).Lsh(big.NewInt(1), 128)
	tests := []struct {
		name   string
		mutate func(*fakeBatchCaller)
	}{
		{"slot0 bad hex", func(c *fakeBatchCaller) {
			c.setOut(testCallData(t, SelectorSlot0, ""), "0xnothex")
		}},
		{"liquidity over uint128", func(c *fakeBatchCaller) {
			c.setOut(testCallData(t, SelectorLiquidity, ""), liquidityResult(overflow))
		}},
		{"bitmap bad hex", func(c *fakeBatchCaller) {
			c.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), "0xnothex")
		}},
		{"ticks too short", func(c *fakeBatchCaller) {
			c.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), bitmapResult(big.NewInt(2)))
			c.setOut(testCallData(t, SelectorTicks, signedArgumentHex(10)), liquidityResult(big.NewInt(1)))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeBatchCaller()
			setDeepResponses(t, caller)
			test.mutate(caller)

			_, err := Load(context.Background(), caller, loaderSpec(), "0x10", 2)
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadV3BatchedResultCountMismatch(t *testing.T) {
	caller := newFakeBatchCaller()
	setDeepResponses(t, caller)
	caller.dropResults = true

	_, err := Load(context.Background(), caller, loaderSpec(), "0x10", 2)
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

type sequentialOnlyCaller struct {
	inner *fakeCaller
	calls int
}

var _ pool.ContractCaller = (*sequentialOnlyCaller)(nil)

func (s *sequentialOnlyCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	s.calls++
	return s.inner.CallContract(ctx, to, data, block, out)
}

func TestLoadV3UsesSequentialPathWithoutBatchCaller(t *testing.T) {
	inner := validLoaderCaller(t)
	caller := &sequentialOnlyCaller{inner: inner}

	if _, err := Load(context.Background(), caller, loaderSpec(), "0x30", 1); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if caller.calls != 5 {
		t.Fatalf("sequential calls = %d, want 5", caller.calls)
	}
}

func recordedCalls(calls []pool.ContractCall) []recordedCall {
	recorded := make([]recordedCall, len(calls))
	for i, call := range calls {
		recorded[i] = recordedCall{to: call.To, data: call.Data, block: call.Block}
	}
	return recorded
}

func setEmptyBitmapBatch(t *testing.T, caller *fakeBatchCaller, positions ...int16) {
	t.Helper()
	for _, position := range positions {
		caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(int64(position))), "0x")
	}
}

func assertLoadedEqual(t *testing.T, got, want Loaded) {
	t.Helper()
	if got.State.Key != want.State.Key {
		t.Fatalf("key = %+v, want %+v", got.State.Key, want.State.Key)
	}
	if got.State.Token0 != want.State.Token0 || got.State.Token1 != want.State.Token1 {
		t.Fatalf("tokens = %s/%s, want %s/%s", got.State.Token0, got.State.Token1, want.State.Token0, want.State.Token1)
	}
	if got.State.Fee != want.State.Fee || got.State.TickSpacing != want.State.TickSpacing {
		t.Fatalf("fee/spacing = %d/%d, want %d/%d", got.State.Fee, got.State.TickSpacing, want.State.Fee, want.State.TickSpacing)
	}
	if got.State.SqrtPriceX96.Cmp(want.State.SqrtPriceX96) != 0 {
		t.Fatalf("sqrt price = %s, want %s", got.State.SqrtPriceX96, want.State.SqrtPriceX96)
	}
	if got.State.Tick != want.State.Tick {
		t.Fatalf("tick = %d, want %d", got.State.Tick, want.State.Tick)
	}
	if got.State.Liquidity.Cmp(want.State.Liquidity) != 0 {
		t.Fatalf("liquidity = %s, want %s", got.State.Liquidity, want.State.Liquidity)
	}
	if got.Deep.WindowMin != want.Deep.WindowMin || got.Deep.WindowMax != want.Deep.WindowMax {
		t.Fatalf("window = [%d, %d], want [%d, %d]",
			got.Deep.WindowMin, got.Deep.WindowMax, want.Deep.WindowMin, want.Deep.WindowMax)
	}

	if len(got.Deep.Bitmap) != len(want.Deep.Bitmap) {
		t.Fatalf("bitmap entries = %d, want %d", len(got.Deep.Bitmap), len(want.Deep.Bitmap))
	}
	for position, word := range want.Deep.Bitmap {
		gotWord, ok := got.Deep.Bitmap[position]
		if !ok || gotWord.Cmp(word) != 0 {
			t.Fatalf("bitmap[%d] = %v, want %s", position, gotWord, word)
		}
	}
	if len(got.Deep.Ticks) != len(want.Deep.Ticks) {
		t.Fatalf("ticks entries = %d, want %d", len(got.Deep.Ticks), len(want.Deep.Ticks))
	}
	for tick, info := range want.Deep.Ticks {
		gotInfo, ok := got.Deep.Ticks[tick]
		if !ok {
			t.Fatalf("tick %d missing", tick)
		}
		if gotInfo.LiquidityGross.Cmp(info.LiquidityGross) != 0 || gotInfo.LiquidityNet.Cmp(info.LiquidityNet) != 0 {
			t.Fatalf("tick %d = %+v, want %+v", tick, gotInfo, info)
		}
	}
}
