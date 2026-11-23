package bootstrap

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

var (
	testToken0 = domain.Address{0x01}
	testToken1 = domain.Address{0x02}
)

type fakeKey struct {
	to   domain.Address
	data string
}

type fakeResponse struct {
	out string
	err error
}

type recordedCall struct {
	to    domain.Address
	data  []byte
	block string
}

// fakeCaller is safe for the concurrent pool loads seedAll runs.
type fakeCaller struct {
	mu        sync.Mutex
	responses map[fakeKey]fakeResponse
	calls     []recordedCall
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{responses: make(map[fakeKey]fakeResponse)}
}

func (f *fakeCaller) setOut(to domain.Address, data []byte, out string) {
	f.responses[fakeKey{to: to, data: hex.EncodeToString(data)}] = fakeResponse{out: out}
}

func (f *fakeCaller) setError(to domain.Address, data []byte, err error) {
	f.responses[fakeKey{to: to, data: hex.EncodeToString(data)}] = fakeResponse{err: err}
}

func (f *fakeCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{to: to, data: append([]byte(nil), data...), block: block})
	response, ok := f.responses[fakeKey{to: to, data: hex.EncodeToString(data)}]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("fake caller: unexpected call to %s with data %x", to, data)
	}
	if response.err != nil {
		return response.err
	}
	*out = response.out
	return nil
}

func selectorBytes(t *testing.T, selector string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(selector, "0x"))
	if err != nil {
		t.Fatalf("decode selector %s: %v", selector, err)
	}
	return raw
}

// v4Call builds a StateView call: the selector, the pool ID, and any static
// arguments.
func v4Call(t *testing.T, selector string, poolID domain.Hash, arguments ...[]byte) []byte {
	t.Helper()
	parts := make([][]byte, 0, len(arguments)+1)
	parts = append(parts, poolID[:])
	parts = append(parts, arguments...)
	return callData(selectorBytes(t, selector), parts...)
}

func callData(selector []byte, arguments ...[]byte) []byte {
	data := append([]byte(nil), selector...)
	for _, argument := range arguments {
		data = append(data, argument...)
	}
	return data
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
	return word(encoded)
}

func hexResult(words ...[]byte) string {
	data := make([]byte, 0, 32*len(words))
	for _, w := range words {
		data = append(data, w...)
	}
	return "0x" + hex.EncodeToString(data)
}

func reservesOut(reserve0, reserve1 int64) string {
	return hexResult(word(big.NewInt(reserve0)), word(big.NewInt(reserve1)))
}

func slot0Out(sqrtPriceX96 *big.Int, tick int64) string {
	return hexResult(word(sqrtPriceX96), signedWord(tick))
}

func liquidityOut(liquidity int64) string {
	return hexResult(word(big.NewInt(liquidity)))
}

// globalStateOut encodes the six static words of Camelot's globalState:
// price, tick, fee, and three pool flags the loader ignores.
func globalStateOut(sqrtPriceX96 *big.Int, tick, fee int64) string {
	return hexResult(
		word(sqrtPriceX96),
		signedWord(tick),
		word(big.NewInt(fee)),
		word(big.NewInt(0)),
		word(big.NewInt(0)),
		word(big.NewInt(0)),
	)
}

// algebraTicksOut encodes the eight-word Camelot ticks response. Only the first
// two words, liquidityTotal and liquidityDelta, are read by the loader.
func algebraTicksOut(gross, net int64) string {
	return hexResult(
		word(big.NewInt(gross)),
		signedWord(net),
		word(big.NewInt(0)),
		word(big.NewInt(0)),
		word(big.NewInt(0)),
		word(big.NewInt(0)),
		word(big.NewInt(0)),
		word(big.NewInt(0)),
	)
}

func q96() *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), 96)
}

// testBlockRef builds a valid nonzero ref for Seed. The hash only has to be
// distinct from the zero hash.
func testBlockRef(number uint64) domain.BlockRef {
	return domain.BlockRef{Number: number, Hash: domain.Hash{byte(number)}}
}

// The key helpers mirror the key Seed builds for each test spec.
func v2Key(addr domain.Address) domain.PoolKey {
	return v2Spec(addr).key()
}

func v3Key(addr domain.Address) domain.PoolKey {
	return v3Spec(addr).key()
}

func camelotKey(addr domain.Address) domain.PoolKey {
	return camelotV3Spec(addr).key()
}

func slipstreamKey(addr domain.Address) domain.PoolKey {
	return slipstreamSpec(addr).key()
}

func v4Key(spec Spec) domain.PoolKey {
	return spec.key()
}

// stateViewAddress is the deployed StateView lens the V4 loader calls.
func stateViewAddress(t *testing.T) domain.Address {
	t.Helper()
	addr, err := domain.ParseAddress(uniswapv4.StateViewAddress)
	if err != nil {
		t.Fatalf("parse StateView address: %v", err)
	}
	return addr
}

func v2Spec(addr domain.Address) Spec {
	return Spec{
		Address: addr,
		Token0:  testToken0,
		Token1:  testToken1,
		Kind:    domain.PoolKindV2,
		Fee:     30,
	}
}

func v3Spec(addr domain.Address) Spec {
	return Spec{
		Address:     addr,
		Token0:      testToken0,
		Token1:      testToken1,
		Kind:        domain.PoolKindV3,
		Fee:         3000,
		TickSpacing: 10,
	}
}

func camelotV3Spec(addr domain.Address) Spec {
	spec := v3Spec(addr)
	spec.Kind = domain.PoolKindCamelotV3
	spec.Fee = 100
	return spec
}

func slipstreamSpec(addr domain.Address) Spec {
	spec := v3Spec(addr)
	spec.Kind = domain.PoolKindSlipstream
	return spec
}

// v4Spec is a hooks-free V4 pool behind the real PoolManager with a PoolID that
// matches its identity fields. Fee is the configured LP fee; the loader replaces
// it with slot0's value, but it stays part of the pool ID.
func v4Spec(t *testing.T, fee uint32) Spec {
	t.Helper()
	return withV4PoolID(t, Spec{
		Address:     uniswapv4.PoolManager(),
		Token0:      testToken0,
		Token1:      testToken1,
		Kind:        domain.PoolKindV4,
		Fee:         fee,
		TickSpacing: 10,
	})
}

// withV4PoolID recomputes the pool ID from the spec's current identity fields.
// Tests call it after changing a field the ID commits to.
func withV4PoolID(t *testing.T, spec Spec) Spec {
	t.Helper()
	id, err := uniswapv4.PoolID(spec.Token0, spec.Token1, spec.Fee, spec.TickSpacing, spec.Hooks)
	if err != nil {
		t.Fatalf("PoolID: %v", err)
	}
	spec.PoolID = id
	return spec
}

func withTokens(spec Spec, token0, token1 domain.Address) Spec {
	spec.Token0 = token0
	spec.Token1 = token1
	return spec
}

func withFee(spec Spec, fee uint32) Spec {
	spec.Fee = fee
	return spec
}

func withPoolID(spec Spec, poolID domain.Hash) Spec {
	spec.PoolID = poolID
	return spec
}

func withHooks(spec Spec, hooks domain.Address) Spec {
	spec.Hooks = hooks
	return spec
}

func withAddress(spec Spec, addr domain.Address) Spec {
	spec.Address = addr
	return spec
}

func withTickSpacing(spec Spec, tickSpacing int32) Spec {
	spec.TickSpacing = tickSpacing
	return spec
}

func withShallow(spec Spec) Spec {
	spec.Shallow = true
	return spec
}

func assertDeepState(t *testing.T, got, want pool.DeepState) {
	t.Helper()
	if len(got.Bitmap) != len(want.Bitmap) || len(got.Ticks) != len(want.Ticks) {
		t.Fatalf("deep state = %d words/%d ticks, want %d/%d",
			len(got.Bitmap), len(got.Ticks), len(want.Bitmap), len(want.Ticks))
	}
	for position, wantWord := range want.Bitmap {
		gotWord, ok := got.Bitmap[position]
		if !ok || gotWord.Cmp(wantWord) != 0 {
			t.Fatalf("bitmap word %d = %v, want %v", position, gotWord, wantWord)
		}
	}
	for tick, info := range want.Ticks {
		gotInfo, ok := got.Ticks[tick]
		if !ok || gotInfo.LiquidityGross.Cmp(info.LiquidityGross) != 0 || gotInfo.LiquidityNet.Cmp(info.LiquidityNet) != 0 {
			t.Fatalf("tick %d = %+v, want %+v", tick, gotInfo, info)
		}
	}
}
