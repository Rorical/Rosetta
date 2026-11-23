package catalog

import (
	"context"
	"fmt"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
)

type v4CatalogReader struct {
	*catalogReader
	manager, view domain.Address
}

func (r *v4CatalogReader) V4Deployment() (domain.Address, domain.Address) { return r.manager, r.view }
func TestMixedV2V4CatalogNativeCurrencyAndPoolIdentities(t *testing.T) {
	a, b, factory, manager, view, pair := domain.Address{1}, domain.Address{2}, domain.Address{3}, domain.Address{4}, domain.Address{5}, domain.Address{6}
	cfg := Config{ChainID: 10, Tokens: []domain.Address{{}, a, b}, Factories: []Factory{{Address: factory, Kind: domain.PoolKindV2, Fees: []uint32{3000}}}, V4: &V4Config{Manager: manager, StateView: view, Tiers: []V4Tier{{Fee: 500, TickSpacing: 10}}}}
	base := &catalogReader{chain: 10, calls: map[string][]byte{}, codes: map[domain.Address]domain.Hash{a: {1}, b: {1}, factory: {1}, manager: {1}, view: {1}, pair: {1}}}
	reader := &v4CatalogReader{catalogReader: base, manager: manager, view: view}
	put := func(to domain.Address, input, output []byte) { base.calls[to.Hex()+fmt.Sprintf("/%x", input)] = output }
	addressWord := func(a domain.Address) []byte { return append(make([]byte, 12), a[:]...) }
	put(factory, callData("getPair(address,address)", a, b, 0)[:68], addressWord(pair))
	for name, value := range map[string]domain.Address{"factory()": factory, "token0()": a, "token1()": b} {
		put(pair, selector(name), addressWord(value))
	}
	put(pair, selector("getReserves()"), append(append(word(1000), word(2000)...), word(1)...))
	put(view, selector("poolManager()"), addressWord(manager))
	var initialized domain.Hash
	for i, token0 := range cfg.Tokens {
		for _, token1 := range cfg.Tokens[i+1:] {
			id, err := uniswapv4.PoolID(token0, token1, 500, 10, domain.Address{})
			if err != nil {
				t.Fatal(err)
			}
			slot := make([]byte, 128)
			if token1 == b {
				copy(slot[:32], word(1234))
				copy(slot[96:], word(500))
				put(view, append(selector("getLiquidity(bytes32)"), id[:]...), word(1000))
				initialized = id
			}
			put(view, append(selector("getSlot0(bytes32)"), id[:]...), slot)
		}
	}
	pools, err := Resolve(t.Context(), reader, cfg)
	if err != nil || len(pools) != 3 {
		t.Fatal(pools, err)
	}
	if pools[0].Kind != domain.PoolKindV4 || pools[1].Kind != domain.PoolKindV4 || pools[0].PoolID == pools[1].PoolID {
		t.Fatal("V4 identities collapsed behind one manager")
	}
	t.Run("reject manager mismatch", func(t *testing.T) {
		bad := *reader
		bad.manager = domain.Address{99}
		if _, err := Resolve(t.Context(), &bad, cfg); err == nil {
			t.Fatal("accepted another chain deployment")
		}
	})
	t.Run("reject static fee mismatch", func(t *testing.T) {
		key := view.Hex() + fmt.Sprintf("/%x", append(selector("getSlot0(bytes32)"), initialized[:]...))
		saved := base.calls[key]
		bad := append([]byte(nil), saved...)
		copy(bad[96:], word(3000))
		base.calls[key] = bad
		defer func() { base.calls[key] = saved }()
		if _, err := Resolve(context.Background(), reader, cfg); err == nil {
			t.Fatal("wrong V4 key fee accepted")
		}
	})
	t.Run("reject V2 token mismatch", func(t *testing.T) {
		key := pair.Hex() + fmt.Sprintf("/%x", selector("token0()"))
		saved := base.calls[key]
		base.calls[key] = addressWord(b)
		defer func() { base.calls[key] = saved }()
		if _, err := Resolve(t.Context(), reader, cfg); err == nil {
			t.Fatal("wrong V2 pair accepted")
		}
	})
}
