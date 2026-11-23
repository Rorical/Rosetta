package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestSeedLoadsManyPoolsConcurrently(t *testing.T) {
	caller := newFakeCaller()
	var specs []Spec
	for i := range 3 * seedParallelism {
		addr := domain.Address{0xb0, byte(i)}
		caller.setOut(addr, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(int64(100+i), int64(200+i)))
		specs = append(specs, v2Spec(addr))
	}
	store := state.New()
	if err := Seed(context.Background(), caller, store, specs, testBlockRef(0x30), 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for i, spec := range specs {
		got, ok := store.Pool(v2Key(spec.Address))
		if !ok || got.Reserve0.Int64() != int64(100+i) || got.Reserve1.Int64() != int64(200+i) {
			t.Fatalf("pool %d = %+v, %v", i, got, ok)
		}
	}
}

func TestSeedReportsFirstFailureInSpecOrder(t *testing.T) {
	caller := newFakeCaller()
	var specs []Spec
	for i := range 12 {
		addr := domain.Address{0xc0, byte(i)}
		specs = append(specs, v2Spec(addr))
		if i == 3 || i == 9 {
			caller.setError(addr, selectorBytes(t, uniswapv2.SelectorGetReserves), errors.New("unavailable"))
			continue
		}
		caller.setOut(addr, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(1, 2))
	}
	store := state.New()
	err := Seed(context.Background(), caller, store, specs, testBlockRef(0x31), 1)
	if err == nil || !strings.Contains(err.Error(), specs[3].Address.Hex()) {
		t.Fatalf("Seed error = %v, want the failure of spec 3", err)
	}
	if len(store.Pools()) != 0 {
		t.Fatalf("failed Seed kept %d pools", len(store.Pools()))
	}
	// Every load ran: an early cancel would let a context error hide the cause.
	if len(caller.calls) != len(specs) {
		t.Fatalf("calls = %d, want %d", len(caller.calls), len(specs))
	}
}
