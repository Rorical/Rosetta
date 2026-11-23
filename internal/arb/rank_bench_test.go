package arb

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Includes dirty indexing, state reads, funding rotation, coarse arithmetic and
// bounded admission. Exact sizing, EVM and network I/O are excluded.
func BenchmarkDirtyRank(b *testing.B) {
	for _, depth := range []int{2, 8, 16} {
		b.Run(fmt.Sprintf("256-paths-%d-hop", depth), func(b *testing.B) {
			view := newFakeView()
			cycles := make([]graph.Cycle, 256)
			for i := range cycles {
				keys := make([]domain.PoolKey, depth)
				for j := range keys {
					p := testV2State(1, byte(j+1), byte((j+1)%depth+1), 1_000_000_000, 1_050_000_000)
					binary.BigEndian.PutUint64(p.Key.Address[12:], uint64(i*depth+j+1))
					// All routes share the first pool, as in a hot dirty-pool index.
					if j == 0 {
						binary.BigEndian.PutUint64(p.Key.Address[12:], 1)
					}
					p.Fee = 3000
					keys[j] = p.Key
					view.add(p)
				}
				cycles[i] = graph.Cycle{Start: testAddress(1), Pools: keys, Len: depth}
			}
			table, err := graph.BuildTableFromCycles(cycles)
			if err != nil {
				b.Fatal(err)
			}
			engine, err := New(table, fixedNotional(1000), 8, identityQuoter())
			if err != nil {
				b.Fatal(err)
			}
			bounds := InputBounds{Min: big.NewInt(1), Max: big.NewInt(1_000_000)}
			opts := RankedOptions{
				Bounds:  func(asset domain.Address) (InputBounds, bool) { return bounds, asset == testAddress(1) },
				SwapFee: func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, nil },
			}
			dirty := cycles[0].Pools[:1]
			b.ReportAllocs()
			for b.Loop() {
				selection, err := engine.Rank(b.Context(), view, dirty, opts)
				if err != nil || len(selection.candidates) != 8 || selection.considered != 256 {
					b.Fatal(selection.considered, err)
				}
			}
		})
	}
}
