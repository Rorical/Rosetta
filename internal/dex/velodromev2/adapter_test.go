package velodromev2

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func integer(s string) *big.Int       { n, _ := new(big.Int).SetString(s, 10); return n }
func address(s string) domain.Address { a, _ := domain.ParseAddress(s); return a }

// These are historical eth_call results, including tiny input rounding and
// reserve-sized swaps in both directions. Unit tests perform no network I/O.
func TestHistoricalDeployedQuotes(t *testing.T) {
	raw, err := os.ReadFile("testdata/op-quotes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Pools []struct {
			Pool, Token0, Token1, Scale0, Scale1, Reserve0, Reserve1 string
			Stable                                                   bool
			FeeBPS                                                   uint32 `json:"fee_bps"`
			Quotes                                                   []struct {
				Token, Input string
				Output       *string
				Revert       json.RawMessage
			}
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range fixture.Pools {
		p := pool.State{Key: domain.PoolKey{Address: address(row.Pool), Kind: domain.PoolKindVelodromeV2}, Token0: address(row.Token0), Token1: address(row.Token1), Fee: row.FeeBPS * 100, Reserve0: integer(row.Reserve0), Reserve1: integer(row.Reserve1), Velodrome: &pool.VelodromeState{Factory: factory, Stable: row.Stable, Scale0: integer(row.Scale0), Scale1: integer(row.Scale1)}}
		for _, q := range row.Quotes {
			t.Run(row.Pool+"/"+q.Token+"/"+q.Input, func(t *testing.T) {
				out, err := New().QuoteExactIn(t.Context(), p, pool.DeepState{}, address(q.Token), integer(q.Input))
				if q.Output == nil {
					if err == nil {
						t.Fatalf("expected onchain revert, got %s", out)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if out.String() != *q.Output {
					t.Fatalf("output=%s want=%s", out, *q.Output)
				}
				count++
			})
		}
	}
	if count < 50 {
		t.Fatalf("only %d successful vectors", count)
	}
}
