package solidlyv3_test

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/solidlyv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type recordedFixture struct {
	adapter *solidlyv3.Adapter
	state   pool.State
	deep    pool.DeepState
	tokenIn []domain.Address
	amount  []*big.Int
}

// BenchmarkRecordedQuoteExactIn quotes every recorded on-chain swap of the
// three fixture pools once per iteration, from dust amounts to swaps that
// cross hundreds of thousands of ticks.
func BenchmarkRecordedQuoteExactIn(b *testing.B) {
	files, err := filepath.Glob("testdata/op-*.json")
	if err != nil || len(files) == 0 {
		b.Fatal("no recorded fixtures", err)
	}
	fixtures := make([]recordedFixture, 0, len(files))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			b.Fatal(err)
		}
		var r recorded
		if err := json.Unmarshal(raw, &r); err != nil {
			b.Fatal(err)
		}
		spec := solidlyv3.Spec{Address: mustAddress(b, r.Pool), Token0: mustAddress(b, r.Token0), Token1: mustAddress(b, r.Token1), TickSpacing: r.TickSpacing}
		state, deep, err := solidlyv3.Load(b.Context(), r, spec, fmt.Sprintf("0x%x", r.Block), r.WordRadius)
		if err != nil {
			b.Fatal(file, err)
		}
		adapter, err := solidlyv3.New(state.TickSpacing)
		if err != nil {
			b.Fatal(err)
		}
		fixture := recordedFixture{adapter: adapter, state: state, deep: deep}
		for _, q := range r.Quotes {
			amount, _ := new(big.Int).SetString(q.AmountIn, 10)
			tokenIn := state.Token1
			if q.ZeroForOne {
				tokenIn = state.Token0
			}
			fixture.tokenIn = append(fixture.tokenIn, tokenIn)
			fixture.amount = append(fixture.amount, amount)
		}
		fixtures = append(fixtures, fixture)
	}
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		for _, f := range fixtures {
			for i, amount := range f.amount {
				// Out-of-window quotes are part of the recorded mix and
				// return an error by design.
				_, _ = f.adapter.QuoteExactIn(ctx, f.state, f.deep, f.tokenIn[i], amount)
			}
		}
	}
}
