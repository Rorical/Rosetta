package discovery

import (
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestUnknownSwapProbeChecksFactoryLookupAndBoundsAttempts(t *testing.T) {
	caller := newFakeCaller()
	a, b, factory, address := repeatedAddress(1), repeatedAddress(2), repeatedAddress(3), repeatedAddress(4)
	for name, value := range map[string]domain.Address{"factory()": factory, "token0()": a, "token1()": b} {
		caller.set(address, probeSelector(name), "0x"+hex.EncodeToString(testAddressWord(value)))
	}
	input := append(append(probeSelector("getPair(address,address)"), testAddressWord(a)...), testAddressWord(b)...)
	caller.set(factory, input, "0x"+hex.EncodeToString(testAddressWord(address)))
	unknown := domain.PoolKey{Address: address}
	options := ProbeOptions{Emitters: []Emitter{{SourceUniswapV2, factory}}, Activity: map[domain.PoolKey]Activity{unknown: {Swaps: 3}}, Seen: map[domain.PoolKey]bool{}, Anchor: domain.BlockRef{Number: 10}, Limit: 1, Cooldown: 100}
	probe := ActiveProbe{}
	rows, err := probe.Run(t.Context(), caller, options)
	if err != nil || len(rows) != 1 || rows[0].Spec.Fee != 3000 {
		t.Fatal(rows, err)
	}
	if len(caller.calls) != 4 {
		t.Fatal("unexpected probe calls", len(caller.calls))
	}
	for _, call := range caller.calls {
		if call.Block != "0xa" {
			t.Fatal("metadata was not snapshot-bound")
		}
	}
	if rows, err := probe.Run(t.Context(), caller, options); err != nil || len(rows) != 0 || len(caller.calls) != 4 {
		t.Fatal("negative cooldown was not bounded", rows, err)
	}
	probe = ActiveProbe{}
	caller.set(factory, input, wordHex(big.NewInt(0)))
	rows, err = probe.Run(t.Context(), caller, options)
	if err != nil || len(rows) != 0 {
		t.Fatal("unregistered pool was admitted", rows, err)
	}
}
func TestUnknownSwapProbeStopsOnBudgetAndProviderErrors(t *testing.T) {
	for _, failure := range []error{rpc.ErrCallBudgetExceeded, &rpc.RPCError{Code: -32005, Message: "rate limited"}, errors.New("connection failed")} {
		caller := newFakeCaller()
		address := repeatedAddress(1)
		caller.setError(address, probeSelector("factory()"), failure)
		probe := ActiveProbe{}
		_, err := probe.Run(t.Context(), caller, ProbeOptions{Activity: map[domain.PoolKey]Activity{{Address: address}: {Swaps: 1}}, Seen: map[domain.PoolKey]bool{}, Anchor: domain.BlockRef{Number: 1}, Limit: 1})
		if !errors.Is(err, failure) || len(caller.calls) != 1 {
			t.Fatal("continued after failed provider request", err, len(caller.calls))
		}
	}
}
