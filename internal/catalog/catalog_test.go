package catalog

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

type catalogReader struct {
	calls map[string][]byte
	codes map[domain.Address]domain.Hash
	chain uint64
}

func (r *catalogReader) Position() execution.Position {
	return execution.Position{Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: r.chain}}
}
func (r *catalogReader) CodeHash(_ context.Context, a domain.Address) (domain.Hash, error) {
	return r.codes[a], nil
}
func (r *catalogReader) Nonce(context.Context, domain.Address) (uint64, error) { return 0, nil }
func (r *catalogReader) ReadContract(_ context.Context, a domain.Address, input []byte) ([]byte, error) {
	value, ok := r.calls[a.Hex()+fmt.Sprintf("/%x", input)]
	if !ok {
		return nil, fmt.Errorf("unexpected catalog read")
	}
	return value, nil
}
func TestFactoryCatalogProvenanceAndLiveMetadata(t *testing.T) {
	cfg, err := Load("../../configs/base-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg.V4 = nil
	cfg.Factories = cfg.Factories[:1]
	cfg.Factories[0].Fees = []uint32{500}
	cfg.Tokens = cfg.Tokens[:2]
	a, b := cfg.Tokens[0], cfg.Tokens[1]
	factory := cfg.Factories[0].Address
	target := domain.Address{0x18}
	r := &catalogReader{chain: 8453, calls: make(map[string][]byte), codes: map[domain.Address]domain.Hash{a: {1}, b: {1}, factory: {1}, target: {1}}}
	put := func(to domain.Address, input, result []byte) { r.calls[to.Hex()+fmt.Sprintf("/%x", input)] = result }
	data := append(selector("feeAmountTickSpacing(uint24)"), word(500)...)
	put(factory, data, word(10))
	addressWord := func(a domain.Address) []byte { return append(make([]byte, 12), a[:]...) }
	put(factory, callData("getPool(address,address,uint24)", a, b, 500), addressWord(target))
	for name, value := range map[string]domain.Address{"factory()": factory, "token0()": a, "token1()": b} {
		put(target, selector(name), addressWord(value))
	}
	for name, value := range map[string]uint64{"fee()": 500, "tickSpacing()": 10, "liquidity()": 1000} {
		put(target, selector(name), word(value))
	}
	pools, err := Resolve(t.Context(), r, cfg)
	if err != nil || len(pools) != 1 || pools[0].Address != target || pools[0].TickSpacing != 10 {
		t.Fatal(pools, err)
	}
	for _, fault := range []string{"factory", "token", "spacing", "fee", "liquidity", "chain", "code", "empty"} {
		t.Run(fault, func(t *testing.T) {
			calls := make(map[string][]byte)
			for k, v := range r.calls {
				calls[k] = v
			}
			codes := make(map[domain.Address]domain.Hash)
			for k, v := range r.codes {
				codes[k] = v
			}
			bad := &catalogReader{chain: 8453, calls: calls, codes: codes}
			field := ""
			var value []byte
			switch fault {
			case "factory":
				field = "factory()"
				value = addressWord(a)
			case "token":
				field = "token1()"
				value = addressWord(a)
			case "spacing":
				field = "tickSpacing()"
				value = word(60)
			case "fee":
				field = "fee()"
				value = word(3000)
			case "liquidity":
				field = "liquidity()"
				value = new(big.Int).Lsh(big.NewInt(1), 129).FillBytes(make([]byte, 32))
			case "empty":
				field = "liquidity()"
				value = word(0)
			case "chain":
				bad.chain = 42161
			case "code":
				delete(codes, target)
			}
			if field != "" {
				calls[target.Hex()+fmt.Sprintf("/%x", selector(field))] = value
			}
			if _, err := Resolve(t.Context(), bad, cfg); err == nil {
				t.Fatal("invalid factory catalog accepted")
			}
		})
	}
}
func TestSlipstreamSpacingArrayBounds(t *testing.T) {
	factory := Factory{Address: domain.Address{1}, Kind: domain.PoolKindSlipstream}
	r := &catalogReader{calls: make(map[string][]byte)}
	key := factory.Address.Hex() + fmt.Sprintf("/%x", selector("tickSpacings()"))
	valid := append(append(word(32), word(2)...), append(word(1), word(200)...)...)
	r.calls[key] = valid
	tiers, err := factoryTiers(t.Context(), r, factory)
	if err != nil || len(tiers) != 2 || tiers[1].spacing != 200 {
		t.Fatal(tiers, err)
	}
	for _, raw := range [][]byte{valid[:len(valid)-1], append(append(word(32), word(2)...), append(word(1), word(1)...)...), append(append(word(32), word(1)...), word(32768)...)} {
		r.calls[key] = raw
		if _, err := factoryTiers(t.Context(), r, factory); err == nil {
			t.Fatal("invalid spacing array accepted")
		}
	}
}
