package uniswapv2

import (
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
		Address: domain.Address{0xbb},
		Token0:  testToken0,
		Token1:  testToken1,
		Fee:     30,
	}
}

func withSpec(mutate func(*Spec)) Spec {
	spec := loaderSpec()
	mutate(&spec)
	return spec
}

func getReservesData(t *testing.T) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(SelectorGetReserves, "0x"))
	if err != nil {
		t.Fatalf("decode selector: %v", err)
	}
	return raw
}

func reservesResult(reserve0, reserve1 *big.Int) string {
	return "0x" + hex.EncodeToString(syncData(reserve0, reserve1))
}

func TestLoadV2Reserves(t *testing.T) {
	spec := loaderSpec()
	caller := newFakeCaller()
	caller.setOut(getReservesData(t), reservesResult(big.NewInt(1111), big.NewInt(2222)))

	state, err := Load(context.Background(), caller, spec, "0x10")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Key != (domain.PoolKey{Address: spec.Address, Kind: domain.PoolKindV2}) {
		t.Fatalf("key = %+v", state.Key)
	}
	if state.Token0 != spec.Token0 || state.Token1 != spec.Token1 {
		t.Fatalf("tokens = %s/%s", state.Token0, state.Token1)
	}
	if state.Fee != spec.Fee {
		t.Fatalf("fee = %d, want %d", state.Fee, spec.Fee)
	}
	assertSyncReserve(t, "reserve0", state.Reserve0, 1111)
	assertSyncReserve(t, "reserve1", state.Reserve1, 2222)

	if len(caller.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(caller.calls))
	}
	call := caller.calls[0]
	if call.to != spec.Address {
		t.Fatalf("call to %s, want %s", call.to, spec.Address)
	}
	if call.block != "0x10" {
		t.Fatalf("call block = %q, want 0x10", call.block)
	}
	if got := hex.EncodeToString(call.data); got != strings.TrimPrefix(SelectorGetReserves, "0x") {
		t.Fatalf("call data = %s, want selector only", got)
	}
}

func TestLoadV2ZeroReserves(t *testing.T) {
	caller := newFakeCaller()
	caller.setOut(getReservesData(t), reservesResult(big.NewInt(0), big.NewInt(0)))

	state, err := Load(context.Background(), caller, loaderSpec(), "0x11")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Reserve0.Sign() != 0 || state.Reserve1.Sign() != 0 {
		t.Fatalf("reserves = %s/%s, want 0/0", state.Reserve0, state.Reserve1)
	}
}

func TestLoadV2MalformedResults(t *testing.T) {
	tests := []struct {
		name string
		out  string
	}{
		{"empty", ""},
		{"bad hex", "0xnothex"},
		{"one word", "0x" + hex.EncodeToString(syncData(big.NewInt(1), big.NewInt(2))[:32])},
		{"partial word", "0x0102"},
		{"reserve0 over uint112", reservesResult(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(0))},
		{"reserve1 over uint112", reservesResult(big.NewInt(0), new(big.Int).Lsh(big.NewInt(1), 112))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeCaller()
			caller.setOut(getReservesData(t), test.out)
			_, err := Load(context.Background(), caller, loaderSpec(), "0x12")
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadV2CallerError(t *testing.T) {
	errRPC := errors.New("rpc unavailable")
	caller := newFakeCaller()
	caller.setError(getReservesData(t), errRPC)

	_, err := Load(context.Background(), caller, loaderSpec(), "0x13")
	if !errors.Is(err, errRPC) {
		t.Fatalf("error = %v, want %v", err, errRPC)
	}
}

func TestLoadV2Validation(t *testing.T) {
	caller := newFakeCaller()
	tests := []struct {
		name   string
		caller pool.ContractCaller
		spec   Spec
		block  string
	}{
		{"nil caller", nil, loaderSpec(), "0x1"},
		{"zero pool address", caller, withSpec(func(s *Spec) { s.Address = domain.Address{} }), "0x1"},
		{"zero token0", caller, withSpec(func(s *Spec) { s.Token0 = domain.Address{} }), "0x1"},
		{"zero token1", caller, withSpec(func(s *Spec) { s.Token1 = domain.Address{} }), "0x1"},
		{"equal tokens", caller, withSpec(func(s *Spec) { s.Token1 = s.Token0 }), "0x1"},
		{"fee at denominator", caller, withSpec(func(s *Spec) { s.Fee = maxFeePips }), "0x1"},
		{"empty block", caller, loaderSpec(), ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(context.Background(), test.caller, test.spec, test.block)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestLoadV2CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	caller := newFakeCaller()

	_, err := Load(ctx, caller, loaderSpec(), "0x1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("calls = %d, want none", len(caller.calls))
	}
}

func TestGetReservesSelectorDecodes(t *testing.T) {
	if len(getReservesCall) != 4 {
		t.Fatalf("decoded length = %d, want 4", len(getReservesCall))
	}
	if got := "0x" + hex.EncodeToString(getReservesCall); got != SelectorGetReserves {
		t.Fatalf("decoded selector = %s, want %s", got, SelectorGetReserves)
	}
}
