package velodromev2

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type fixtureCaller struct {
	t                 *testing.T
	metadata, factory string
	reads             int
	fee               uint64
}

func (c *fixtureCaller) CallContract(_ context.Context, to domain.Address, data []byte, block string, out *string) error {
	c.t.Helper()
	c.reads++
	if block != fmt.Sprintf("0x%x", uint64(158954685)) {
		return fmt.Errorf("wrong pinned block: %s", block)
	}
	switch hex.EncodeToString(data[:4]) {
	case "392f37e9":
		*out = c.metadata
	case "c45a0155":
		*out = c.factory
	case "cc56b2c5":
		if to != factory || len(data) != 68 || data[67] != 1 {
			c.t.Fatal("wrong stable fee request", to, data)
		}
		*out = fmt.Sprintf("0x%064x", c.fee)
	default:
		return fmt.Errorf("unexpected selector %x", data)
	}
	return nil
}
func TestLoadAndFeeRefreshUseDeployedMetadata(t *testing.T) {
	raw, e := os.ReadFile("testdata/op-quotes.json")
	if e != nil {
		t.Fatal(e)
	}
	var v struct {
		Pools []struct{ Pool, Token0, Token1, Metadata, Factory string }
	}
	if e = json.Unmarshal(raw, &v); e != nil {
		t.Fatal(e)
	}
	row := v.Pools[0]
	c := &fixtureCaller{t: t, metadata: row.Metadata, factory: row.Factory, fee: 5}
	// The fixture height is 158954685.
	tag := fmt.Sprintf("0x%x", uint64(158954685))
	p, e := Load(t.Context(), c, address(row.Pool), address(row.Token0), address(row.Token1), tag)
	if e != nil {
		t.Fatal(e)
	}
	if p.Fee != 500 || p.Velodrome.Scale0.Cmp(big.NewInt(1_000_000)) != 0 || !p.Velodrome.Stable {
		t.Fatal("wrong metadata", p)
	}
	c.reads = 0
	c.fee = 0
	updated, e := LoadFee(t.Context(), c, p, tag)
	if e != nil {
		t.Fatal(e)
	}
	if updated.Fee != 0 || p.Fee != 500 || c.reads != 1 {
		t.Fatal("zero fee or immutable metadata cache lost", updated, c.reads)
	}
	c.factory = fmt.Sprintf("0x%064x", 1)
	if _, e = Load(t.Context(), c, p.Key.Address, p.Token0, p.Token1, tag); e == nil {
		t.Fatal("unverified factory accepted")
	}
	p.Reserve0 = new(big.Int).Lsh(big.NewInt(1), 255)
	if _, e = New().QuoteExactIn(t.Context(), p, pool.DeepState{}, p.Token0, big.NewInt(1)); e == nil {
		t.Fatal("Solidity overflow was quoted")
	}
}

// legacyCaller serves a Velodrome V1 pair: no factory() getter, membership
// through the V1 factory's isPair, and one fee per curve.
type legacyCaller struct {
	t        *testing.T
	metadata string
	pair     domain.Address
}

func (c *legacyCaller) CallContract(_ context.Context, to domain.Address, data []byte, _ string, out *string) error {
	switch hex.EncodeToString(data[:4]) {
	case "392f37e9":
		*out = c.metadata
	case "c45a0155":
		return fmt.Errorf("%w: execution reverted", pool.ErrCallRejected)
	case "e5e31b13":
		if to != legacyFactory || domain.Address(data[16:36]) != c.pair {
			c.t.Fatal("membership asked of the wrong factory or pair")
		}
		*out = fmt.Sprintf("0x%064x", 1)
	case "512b45ea":
		if to != legacyFactory || len(data) != 36 {
			c.t.Fatal("wrong V1 fee request", to, data)
		}
		*out = fmt.Sprintf("0x%064x", 5)
	default:
		return fmt.Errorf("unexpected selector %x", data)
	}
	return nil
}

func TestLoadRecognizesVelodromeV1Pairs(t *testing.T) {
	pair, t0, t1 := address("0x79c912fef520be002c2b6e57ec4324e260f38e50"), domain.Address{0x0a}, domain.Address{0x0b}
	word := func(v *big.Int) string { return fmt.Sprintf("%064x", v) }
	metadata := "0x" + word(big.NewInt(1e18)) + word(big.NewInt(1e6)) + word(big.NewInt(1e18)) + word(big.NewInt(3e9)) + word(new(big.Int)) +
		word(new(big.Int).SetBytes(t0[:])) + word(new(big.Int).SetBytes(t1[:]))
	p, err := Load(t.Context(), &legacyCaller{t: t, metadata: metadata, pair: pair}, pair, t0, t1, "0x1")
	if err != nil {
		t.Fatal(err)
	}
	if p.Velodrome.Factory != LegacyFactory() || p.Fee != 500 || p.Velodrome.Stable {
		t.Fatalf("V1 pair loaded as %+v fee %d", p.Velodrome, p.Fee)
	}
	if _, err := New().QuoteExactIn(t.Context(), p, pool.DeepState{}, t0, big.NewInt(1e15)); err != nil {
		t.Fatal("loaded V1 pair cannot be quoted", err)
	}
}
