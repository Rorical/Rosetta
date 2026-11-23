package arb

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Complete dirty ranking against recorded OP pools. Stable inputs expose
// cross-frame memo reuse; changing a directional fee exercises invalidation.
func BenchmarkRecordedCoarse(b *testing.B) {
	for _, mode := range []string{"stable", "fee-changes"} {
		b.Run(mode, func(b *testing.B) {
			engine, view, dirty, opts := recordedSearchFixture(b)
			original := opts.SwapFee
			var adjustment uint32
			opts.SwapFee = func(p pool.State, token domain.Address) (uint32, error) {
				fee, err := original(p, token)
				if p.Key == dirty[0] && token == p.Token0 {
					fee += adjustment
				}
				return fee, err
			}
			var cache rankCache
			cache.bind(engine.compiled, 1)
			if _, err := engine.rankWithWorkers(b.Context(), view, dirty, opts, &cache, 2); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if mode == "fee-changes" {
					adjustment = 10 - adjustment
				}
				if _, err := engine.rankWithWorkers(b.Context(), view, dirty, opts, &cache, 2); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
