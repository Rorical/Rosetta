package catalog

import (
	"fmt"
	"testing"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/domain"
)

func TestBackgroundCamelotUsesPairLookupAndDirectionalMetadata(t *testing.T) {
	a, b, factory, address := domain.Address{1}, domain.Address{2}, domain.Address{3}, domain.Address{4}
	reader := &catalogReader{calls: map[string][]byte{}, codes: map[domain.Address]domain.Hash{address: {1}}}
	put := func(to domain.Address, input, result []byte) {
		reader.calls[to.Hex()+fmt.Sprintf("/%x", input)] = result
	}
	addressWord := func(value domain.Address) []byte { return append(make([]byte, 12), value[:]...) }
	put(factory, callData("poolByPair(address,address)", a, b, 0)[:68], addressWord(address))
	for signature, value := range map[string]domain.Address{"factory()": factory, "token0()": a, "token1()": b} {
		put(address, selector(signature), addressWord(value))
	}
	global := []byte{}
	for _, value := range []uint64{100000, 0, 500, 3000, 0, 0, 0, 1} {
		global = append(global, word(value)...)
	}
	put(address, selector("globalState()"), global)
	put(address, selector("tickSpacing()"), word(60))
	put(address, selector("liquidity()"), word(1000000))
	spec, active, err := VerifyPool(t.Context(), reader, Factory{Address: factory, Kind: domain.PoolKindCamelotV3}, bootstrap.Spec{Address: address, Kind: domain.PoolKindCamelotV3, Token0: a, Token1: b})
	if err != nil || !active || spec.Fee != 500 || spec.TickSpacing != 60 {
		t.Fatal(spec, active, err)
	}
}
