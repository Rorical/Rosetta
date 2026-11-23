package velodromev2

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Velodrome V1 pairs quoted on OP block 159022333 by their own getAmountOut.
// V1 shares V2's invariants; only the factory and its fee getter differ.
func TestLegacyPairsMatchOnChainQuotes(t *testing.T) {
	raw, err := os.ReadFile("testdata/v1_quotes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Factory string
		FeeBPS  struct{ Stable, Volatile uint32 } `json:"fee_bps"`
		Pools   []struct {
			Address, Token0, Token1, Scale0, Scale1, Reserve0, Reserve1 string
			Stable                                                      bool
			Quotes                                                      []struct {
				TokenIn   string `json:"token_in"`
				AmountIn  string `json:"amount_in"`
				AmountOut string `json:"amount_out"`
			}
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	integer := func(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }
	address := func(s string) domain.Address { a, _ := domain.ParseAddress(s); return a }
	checked := 0
	for _, p := range fixture.Pools {
		fee := fixture.FeeBPS.Volatile
		if p.Stable {
			fee = fixture.FeeBPS.Stable
		}
		state := pool.State{Key: domain.PoolKey{Address: address(p.Address), Kind: domain.PoolKindVelodromeV2}, Token0: address(p.Token0), Token1: address(p.Token1),
			Reserve0: integer(p.Reserve0), Reserve1: integer(p.Reserve1), Fee: fee * 100,
			Velodrome: &pool.VelodromeState{Factory: legacyFactory, Stable: p.Stable, Scale0: integer(p.Scale0), Scale1: integer(p.Scale1)}}
		for _, q := range p.Quotes {
			got, err := New().QuoteExactIn(t.Context(), state, pool.DeepState{}, address(q.TokenIn), integer(q.AmountIn))
			if err != nil {
				t.Fatalf("%s stable=%v: %v", p.Address, p.Stable, err)
			}
			if got.Cmp(integer(q.AmountOut)) != 0 {
				t.Errorf("%s stable=%v in %s: got %s want %s", p.Address[:10], p.Stable, q.AmountIn, got, q.AmountOut)
			}
			checked++
		}
	}
	if checked < 40 {
		t.Fatal("fixture too small", checked)
	}
}
