package catalog

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/domain"
)

func TestVelodromeCatalogUsesBooleanCurveAndLiveFee(t *testing.T) {
	raw, err := os.ReadFile("../dex/velodromev2/testdata/op-quotes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Pools []struct{ Pool, Token0, Token1, Metadata, Factory string }
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	row := fixture.Pools[0]
	target, _ := domain.ParseAddress(row.Pool)
	t0, _ := domain.ParseAddress(row.Token0)
	t1, _ := domain.ParseAddress(row.Token1)
	factory, _ := domain.ParseAddress("0xf1046053aa5682b4f9a81b5481394da16be5ff5a")
	r := &catalogReader{chain: 10, calls: map[string][]byte{}, codes: map[domain.Address]domain.Hash{target: {1}}}
	put := func(to domain.Address, input, result []byte) { r.calls[to.Hex()+fmt.Sprintf("/%x", input)] = result }
	addressWord := func(a domain.Address) []byte { return append(make([]byte, 12), a[:]...) }
	put(target, selector("factory()"), addressWord(factory))
	put(target, selector("token0()"), addressWord(t0))
	put(target, selector("token1()"), addressWord(t1))
	put(target, selector("stable()"), word(1))
	metadata, _ := hex.DecodeString(row.Metadata[2:])
	put(target, selector("metadata()"), metadata)
	feeInput := append(selector("getFee(address,bool)"), addressWord(target)...)
	feeInput = append(feeInput, word(1)...)
	put(factory, feeInput, word(5))
	lookup := callData("getPool(address,address,bool)", t0, t1, 1)
	put(factory, lookup, addressWord(target))
	spec, active, err := VerifyPool(t.Context(), r, Factory{Address: factory, Kind: domain.PoolKindVelodromeV2}, bootstrap.Spec{Address: target, Kind: domain.PoolKindVelodromeV2, Token0: t0, Token1: t1})
	if err != nil || !active || spec.Fee != 500 || spec.Velodrome == nil || !spec.Velodrome.Stable {
		t.Fatal(spec, active, err)
	}
	put(factory, lookup, addressWord(domain.Address{9}))
	if _, _, err = VerifyPool(t.Context(), r, Factory{Address: factory, Kind: spec.Kind}, spec); err == nil {
		t.Fatal("foreign pool admitted")
	}
	cfg, err := Load("../../configs/optimism-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Tokens) < 22 {
		t.Fatal("long-tail assets absent")
	}
}

// A V1 pair has no factory() getter and the V1 factory resolves pairs with
// getPair(address,address,bool), not getPool.
func TestVelodromeCatalogAdmitsV1Pair(t *testing.T) {
	raw, err := os.ReadFile("../dex/velodromev2/testdata/v1_quotes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Factory string
		Pools   []struct {
			Address, Token0, Token1, Scale0, Scale1, Reserve0, Reserve1 string
			Stable                                                      bool
		}
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	row := fixture.Pools[0]
	target, _ := domain.ParseAddress(row.Address)
	t0, _ := domain.ParseAddress(row.Token0)
	t1, _ := domain.ParseAddress(row.Token1)
	factory, _ := domain.ParseAddress(fixture.Factory)
	r := &catalogReader{chain: 10, calls: map[string][]byte{}, codes: map[domain.Address]domain.Hash{target: {1}}}
	put := func(to domain.Address, input, result []byte) { r.calls[to.Hex()+fmt.Sprintf("/%x", input)] = result }
	addressWord := func(a domain.Address) []byte { return append(make([]byte, 12), a[:]...) }
	decimal := func(s string) []byte { v, _ := new(big.Int).SetString(s, 10); return v.FillBytes(make([]byte, 32)) }
	stable := uint64(0)
	if row.Stable {
		stable = 1
	}
	put(target, selector("token0()"), addressWord(t0))
	put(target, selector("token1()"), addressWord(t1))
	put(target, selector("stable()"), word(stable))
	metadata := slices.Concat(decimal(row.Scale0), decimal(row.Scale1), decimal(row.Reserve0), decimal(row.Reserve1), word(stable), addressWord(t0), addressWord(t1))
	put(target, selector("metadata()"), metadata)
	put(factory, append(selector("isPair(address)"), addressWord(target)...), word(1))
	put(factory, append(selector("getFee(bool)"), word(stable)...), word(5))
	lookup := callData("getPair(address,address,bool)", t0, t1, stable)
	put(factory, lookup, addressWord(target))
	spec, active, err := VerifyPool(t.Context(), r, Factory{Address: factory, Kind: domain.PoolKindVelodromeV2}, bootstrap.Spec{Address: target, Kind: domain.PoolKindVelodromeV2, Token0: t0, Token1: t1})
	if err != nil || !active || spec.Fee != 500 || spec.Velodrome == nil || spec.Velodrome.Factory != factory {
		t.Fatal(spec, active, err)
	}
	put(factory, lookup, addressWord(domain.Address{9}))
	if _, _, err = VerifyPool(t.Context(), r, Factory{Address: factory, Kind: spec.Kind}, spec); err == nil {
		t.Fatal("foreign pair admitted")
	}
}
