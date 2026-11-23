package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestSeedV2StoresReserves(t *testing.T) {
	addr := domain.Address{0xa2}
	caller := newFakeCaller()
	caller.setOut(addr, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(1111, 2222))

	store := state.New()
	ref := testBlockRef(0x10)
	if err := Seed(context.Background(), caller, store, []Spec{v2Spec(addr)}, ref, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	got, ok := store.Pool(v2Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after Seed", addr)
	}
	if got.Key != (domain.PoolKey{Address: addr, Kind: domain.PoolKindV2}) {
		t.Fatalf("key = %+v", got.Key)
	}
	if got.Token0 != testToken0 || got.Token1 != testToken1 {
		t.Fatalf("tokens = %s/%s, want %s/%s", got.Token0, got.Token1, testToken0, testToken1)
	}
	if got.Fee != 30 {
		t.Fatalf("fee = %d, want 30", got.Fee)
	}
	if got.Reserve0.Cmp(big.NewInt(1111)) != 0 || got.Reserve1.Cmp(big.NewInt(2222)) != 0 {
		t.Fatalf("reserves = %s/%s, want 1111/2222", got.Reserve0, got.Reserve1)
	}
	if _, ok := store.Deep(v2Key(addr)); ok {
		t.Fatalf("Seed stored deep state for a V2 pool")
	}
	if got := store.Block(); got != ref {
		t.Fatalf("store block = %+v, want %+v", got, ref)
	}

	if len(caller.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(caller.calls))
	}
	if caller.calls[0].to != addr || caller.calls[0].block != "0x10" {
		t.Fatalf("call = %+v, want %s at block 0x10", caller.calls[0], addr)
	}
}

func TestSeedV3StoresShallowAndEmptyDeep(t *testing.T) {
	addr := domain.Address{0xa3}
	caller := newFakeCaller()
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorSlot0), slot0Out(q96(), 0))
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorLiquidity), liquidityOut(1_000_000))
	bitmapSelector := selectorBytes(t, uniswapv3.SelectorTickBitmap)
	for _, position := range []int64{-1, 0, 1} {
		caller.setOut(addr, callData(bitmapSelector, signedWord(position)), "0x")
	}

	store := state.New()
	ref := testBlockRef(0x11)
	if err := Seed(context.Background(), caller, store, []Spec{v3Spec(addr)}, ref, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	got, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after Seed", addr)
	}
	if got.Key != (domain.PoolKey{Address: addr, Kind: domain.PoolKindV3}) {
		t.Fatalf("key = %+v", got.Key)
	}
	if got.SqrtPriceX96.Cmp(q96()) != 0 {
		t.Fatalf("sqrt price = %s, want %s", got.SqrtPriceX96, q96())
	}
	if got.Tick != 0 {
		t.Fatalf("tick = %d, want 0", got.Tick)
	}
	if got.Liquidity.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("liquidity = %s, want 1000000", got.Liquidity)
	}
	if got.Fee != 3000 || got.TickSpacing != 10 {
		t.Fatalf("fee/spacing = %d/%d, want 3000/10", got.Fee, got.TickSpacing)
	}

	deep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after Seed", addr)
	}
	if deep.Bitmap == nil || deep.Ticks == nil {
		t.Fatalf("deep maps = %v/%v, want non-nil empty maps", deep.Bitmap, deep.Ticks)
	}
	if len(deep.Bitmap) != 0 || len(deep.Ticks) != 0 {
		t.Fatalf("deep state = %d words/%d ticks, want empty", len(deep.Bitmap), len(deep.Ticks))
	}
	if deep.LoadedBlock != ref {
		t.Fatalf("loaded block = %+v, want %+v", deep.LoadedBlock, ref)
	}
	if got := store.Block(); got != ref {
		t.Fatalf("store block = %+v, want %+v", got, ref)
	}

	if len(caller.calls) != 5 {
		t.Fatalf("calls = %d, want 5", len(caller.calls))
	}
}

// A spacing-1 pool loads nine words on each side so its window covers a 20%
// price move; the configured radius of one is only a floor.
func TestSeedV3CoversPriceRangeOnFineSpacing(t *testing.T) {
	addr := domain.Address{0xa4}
	caller := newFakeCaller()
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorSlot0), slot0Out(q96(), 0))
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorLiquidity), liquidityOut(1_000_000))
	bitmapSelector := selectorBytes(t, uniswapv3.SelectorTickBitmap)
	for position := int64(-9); position <= 9; position++ {
		caller.setOut(addr, callData(bitmapSelector, signedWord(position)), "0x")
	}
	spec := v3Spec(addr)
	spec.Fee, spec.TickSpacing = 100, 1
	store := state.New()
	if err := Seed(context.Background(), caller, store, []Spec{spec}, testBlockRef(0x12), 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	deep, ok := store.Deep(v3Key(addr))
	if !ok || deep.WindowMin != -9*256 || deep.WindowMax != 10*256-1 {
		t.Fatalf("window = [%d, %d], want [-2304, 2559]", deep.WindowMin, deep.WindowMax)
	}
}

func TestSeedCamelotV3StoresShallowAndDeep(t *testing.T) {
	addr := domain.Address{0xb1}
	caller := newFakeCaller()
	caller.setOut(addr, selectorBytes(t, camelotv3.SelectorGlobalState), globalStateOut(q96(), -7, 500))
	caller.setOut(addr, selectorBytes(t, camelotv3.SelectorLiquidity), liquidityOut(2_000_000))

	// Tick -7 at spacing 10 compresses to -1, so radius 1 covers compressed
	// words -2..0. Algebra's tickTable is indexed by raw tick, so that is raw
	// words -20..9; raw word -1 bit 246 is raw tick -10.
	tickTable := selectorBytes(t, camelotv3.SelectorTickTable)
	for word := int64(-20); word <= 9; word++ {
		if word == -1 {
			continue
		}
		caller.setOut(addr, callData(tickTable, signedWord(word)), "0x")
	}
	table := new(big.Int).SetBit(new(big.Int), 246, 1)
	caller.setOut(addr, callData(tickTable, signedWord(-1)), hexResult(word(table)))
	caller.setOut(addr, callData(selectorBytes(t, camelotv3.SelectorTicks), signedWord(-10)), algebraTicksOut(400, -400))

	store := state.New()
	ref := testBlockRef(0x12)
	if err := Seed(context.Background(), caller, store, []Spec{camelotV3Spec(addr)}, ref, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	got, ok := store.Pool(camelotKey(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after Seed", addr)
	}
	if got.Key != (domain.PoolKey{Address: addr, Kind: domain.PoolKindCamelotV3}) {
		t.Fatalf("key = %+v, want Camelot V3 kind", got.Key)
	}
	if got.SqrtPriceX96.Cmp(q96()) != 0 || got.Tick != -7 {
		t.Fatalf("price/tick = (%s, %d), want (%s, -7)", got.SqrtPriceX96, got.Tick, q96())
	}
	if got.Liquidity.Cmp(big.NewInt(2_000_000)) != 0 {
		t.Fatalf("liquidity = %s, want 2000000", got.Liquidity)
	}
	// The on-chain fee in globalState wins over the spec fallback.
	if got.Fee != 500 || got.TickSpacing != 10 {
		t.Fatalf("fee/spacing = %d/%d, want 500/10", got.Fee, got.TickSpacing)
	}

	deep, ok := store.Deep(camelotKey(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after Seed", addr)
	}
	if len(deep.Bitmap) != 1 || len(deep.Ticks) != 1 {
		t.Fatalf("deep state = %d words/%d ticks, want 1/1", len(deep.Bitmap), len(deep.Ticks))
	}
	if word := deep.Bitmap[-1]; word == nil || word.Bit(255) != 1 {
		t.Fatalf("deep bitmap[-1] = %v, want bit 255", word)
	}
	info, ok := deep.Ticks[-10]
	if !ok || info.LiquidityGross.Cmp(big.NewInt(400)) != 0 || info.LiquidityNet.Cmp(big.NewInt(-400)) != 0 {
		t.Fatalf("deep tick -10 = %+v, want gross 400 net -400", info)
	}
	if deep.LoadedBlock != ref {
		t.Fatalf("loaded block = %+v, want %+v", deep.LoadedBlock, ref)
	}
	if got := store.Block(); got != ref {
		t.Fatalf("store block = %+v, want %+v", got, ref)
	}
	if len(caller.calls) != 33 {
		t.Fatalf("calls = %d, want 33", len(caller.calls))
	}
}

func TestSeedSlipstreamStoresShallowAndDeep(t *testing.T) {
	addr := domain.Address{0xb2}
	caller := newFakeCaller()
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorSlot0), slot0Out(q96(), 0))
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorLiquidity), liquidityOut(1_000_000))
	bitmapSelector := selectorBytes(t, uniswapv3.SelectorTickBitmap)
	for _, position := range []int64{-1, 0, 1} {
		caller.setOut(addr, callData(bitmapSelector, signedWord(position)), "0x")
	}
	caller.setOut(addr, selectorBytes(t, slipstream.SelectorFee), hexResult(word(big.NewInt(3000))))

	store := state.New()
	ref := testBlockRef(0x13)
	if err := Seed(context.Background(), caller, store, []Spec{slipstreamSpec(addr)}, ref, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	got, ok := store.Pool(slipstreamKey(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after Seed", addr)
	}
	if got.Key != (domain.PoolKey{Address: addr, Kind: domain.PoolKindSlipstream}) {
		t.Fatalf("key = %+v, want Slipstream kind", got.Key)
	}
	if got.SqrtPriceX96.Cmp(q96()) != 0 || got.Tick != 0 {
		t.Fatalf("price/tick = (%s, %d), want (%s, 0)", got.SqrtPriceX96, got.Tick, q96())
	}
	if got.Liquidity.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("liquidity = %s, want 1000000", got.Liquidity)
	}
	if got.Fee != 3000 || got.TickSpacing != 10 {
		t.Fatalf("fee/spacing = %d/%d, want 3000/10", got.Fee, got.TickSpacing)
	}

	deep, ok := store.Deep(slipstreamKey(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after Seed", addr)
	}
	if deep.Bitmap == nil || deep.Ticks == nil {
		t.Fatalf("deep maps = %v/%v, want non-nil empty maps", deep.Bitmap, deep.Ticks)
	}
	if len(deep.Bitmap) != 0 || len(deep.Ticks) != 0 {
		t.Fatalf("deep state = %d words/%d ticks, want empty", len(deep.Bitmap), len(deep.Ticks))
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
}

func TestSeedRestoresSnapshotOnLoaderError(t *testing.T) {
	existingAddr := domain.Address{0xe1}
	existing := pool.State{
		Key:      domain.PoolKey{Address: existingAddr, Kind: domain.PoolKindV2},
		Token0:   testToken0,
		Token1:   testToken1,
		Fee:      30,
		Reserve0: big.NewInt(10),
		Reserve1: big.NewInt(20),
	}
	existingDeep := pool.DeepState{
		Bitmap: map[int16]*big.Int{2: big.NewInt(0x10)},
		Ticks:  map[int32]pool.TickInfo{20: {LiquidityGross: big.NewInt(5), LiquidityNet: big.NewInt(5)}},
	}

	store := state.New()
	if err := store.Upsert(existing); err != nil {
		t.Fatalf("Upsert existing: %v", err)
	}
	if err := store.SetDeep(v2Key(existingAddr), existingDeep); err != nil {
		t.Fatalf("SetDeep existing: %v", err)
	}
	store.ClearDirty()

	first := domain.Address{0xa4}
	second := domain.Address{0xa5}
	caller := newFakeCaller()
	caller.setOut(first, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(1, 2))
	loadErr := errors.New("fake caller: rpc unavailable")
	caller.setError(second, selectorBytes(t, uniswapv2.SelectorGetReserves), loadErr)

	err := Seed(context.Background(), caller, store, []Spec{v2Spec(first), v2Spec(second)}, testBlockRef(0x20), 1)
	if !errors.Is(err, loadErr) {
		t.Fatalf("Seed error = %v, want %v", err, loadErr)
	}
	if !strings.Contains(err.Error(), "load v2 pool") {
		t.Fatalf("Seed error = %v, want context naming the v2 load", err)
	}

	for _, addr := range []domain.Address{first, second} {
		if _, ok := store.Pool(v2Key(addr)); ok {
			t.Fatalf("failed Seed kept pool %s", addr)
		}
		if _, ok := store.Deep(v2Key(addr)); ok {
			t.Fatalf("failed Seed kept deep state for %s", addr)
		}
	}
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("failed Seed left dirty pools: %v", dirty)
	}
	if got := store.Block(); got != (domain.BlockRef{}) {
		t.Fatalf("failed Seed recorded head %+v, want the zero block", got)
	}

	got, ok := store.Pool(v2Key(existingAddr))
	if !ok {
		t.Fatalf("failed Seed removed the pre-existing pool %s", existingAddr)
	}
	if got.Key != existing.Key || got.Token0 != existing.Token0 || got.Token1 != existing.Token1 || got.Fee != existing.Fee {
		t.Fatalf("pre-existing pool identity changed: %+v", got)
	}
	if got.Reserve0.Cmp(existing.Reserve0) != 0 || got.Reserve1.Cmp(existing.Reserve1) != 0 {
		t.Fatalf("pre-existing reserves = %s/%s, want 10/20", got.Reserve0, got.Reserve1)
	}
	gotDeep, ok := store.Deep(v2Key(existingAddr))
	if !ok {
		t.Fatalf("failed Seed removed the pre-existing deep state")
	}
	assertDeepState(t, gotDeep, existingDeep)

	if len(caller.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(caller.calls))
	}
}

func TestSeedV3ShallowSkipsDeepLoad(t *testing.T) {
	addr := domain.Address{0xa5}
	caller := newFakeCaller()
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorSlot0), slot0Out(q96(), -887272))
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorLiquidity), liquidityOut(1_000_000))

	spec := v3Spec(addr)
	spec.Shallow = true
	store := state.New()
	ref := testBlockRef(0x12)
	if err := Seed(context.Background(), caller, store, []Spec{spec}, ref, 0); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	got, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after Seed", addr)
	}
	if got.Key != (domain.PoolKey{Address: addr, Kind: domain.PoolKindV3}) {
		t.Fatalf("key = %+v", got.Key)
	}
	if got.Token0 != testToken0 || got.Token1 != testToken1 {
		t.Fatalf("tokens = %s/%s, want %s/%s", got.Token0, got.Token1, testToken0, testToken1)
	}
	if got.SqrtPriceX96.Cmp(q96()) != 0 {
		t.Fatalf("sqrt price = %s, want %s", got.SqrtPriceX96, q96())
	}
	if got.Tick != -887272 {
		t.Fatalf("tick = %d, want -887272", got.Tick)
	}
	if got.Liquidity.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("liquidity = %s, want 1000000", got.Liquidity)
	}
	if got.Fee != 3000 || got.TickSpacing != 10 {
		t.Fatalf("fee/spacing = %d/%d, want 3000/10", got.Fee, got.TickSpacing)
	}
	deep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after shallow Seed", addr)
	}
	if deep.WindowMin != -887272 || deep.WindowMax != -887272 {
		t.Fatalf("window = [%d, %d], want [-887272, -887272]", deep.WindowMin, deep.WindowMax)
	}
	if deep.Bitmap == nil || deep.Ticks == nil {
		t.Fatalf("deep maps = %v/%v, want non-nil empty maps", deep.Bitmap, deep.Ticks)
	}
	if len(deep.Bitmap) != 0 || len(deep.Ticks) != 0 {
		t.Fatalf("deep state = %d words/%d ticks, want empty", len(deep.Bitmap), len(deep.Ticks))
	}
	for _, stale := range store.DeepStale() {
		if stale == v3Key(addr) {
			t.Fatal("shallow seed left the pool deep-stale")
		}
	}
	if got := store.Block(); got != ref {
		t.Fatalf("store block = %+v, want %+v", got, ref)
	}

	if len(caller.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(caller.calls))
	}
	wantCalls := [][]byte{
		selectorBytes(t, uniswapv3.SelectorSlot0),
		selectorBytes(t, uniswapv3.SelectorLiquidity),
	}
	forbidden := [][]byte{
		selectorBytes(t, uniswapv3.SelectorTickBitmap),
		selectorBytes(t, uniswapv3.SelectorTicks),
	}
	for i, call := range caller.calls {
		if call.to != addr || call.block != "0x12" {
			t.Fatalf("call %d = %s at %s, want %s at 0x12", i, call.to.Hex(), call.block, addr.Hex())
		}
		if !bytes.Equal(call.data, wantCalls[i]) {
			t.Fatalf("call %d data = %x, want %x", i, call.data, wantCalls[i])
		}
		for _, selector := range forbidden {
			if bytes.HasPrefix(call.data, selector) {
				t.Fatalf("call %d read deep tick storage", i)
			}
		}
	}
}

func TestSeedV3ShallowRejectsTickZero(t *testing.T) {
	addr := domain.Address{0xa6}
	caller := newFakeCaller()
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorSlot0), slot0Out(q96(), 0))
	caller.setOut(addr, selectorBytes(t, uniswapv3.SelectorLiquidity), liquidityOut(1_000_000))

	spec := v3Spec(addr)
	spec.Shallow = true
	store := state.New()
	err := Seed(context.Background(), caller, store, []Spec{spec}, testBlockRef(0x13), 0)
	if err == nil {
		t.Fatal("shallow seed at tick 0 accepted")
	}
	if !strings.Contains(err.Error(), "tick 0") || !strings.Contains(err.Error(), "full seed") {
		t.Fatalf("error = %v, want the shallow tick-0 limitation", err)
	}
	if _, ok := store.Pool(v3Key(addr)); ok {
		t.Fatal("rejected shallow seed stored the pool")
	}
	if _, ok := store.Deep(v3Key(addr)); ok {
		t.Fatal("rejected shallow seed stored deep state")
	}
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("rejected shallow seed left dirty pools: %v", dirty)
	}
	if got := store.Block(); got != (domain.BlockRef{}) {
		t.Fatalf("store block = %+v, want the zero ref", got)
	}
}
