package remote

import (
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// benchHistory fills the closed windows directly, n of them spread over the
// retention, a third contested by up to two of 60 rivals.
func benchHistory(n int) *bidHistory {
	h := newBidHistory()
	step := bidRetention / time.Duration(n)
	for i := range n {
		pools := []domain.PoolKey{{Address: domain.Address{byte(i % 97)}}, {Address: domain.Address{byte(i % 89), 1}}, {Address: domain.Address{byte(i % 13), 2}}}
		w := bidWindow{pools: pools, start: h.at, last: h.at, worth: uint64(10_000 * (1 + i%300))}
		for j := range i % 3 {
			w.contend(domain.Address{byte((i + j) % 60), 9}, domain.Address{}, uint64(1000*(1+i%500)))
		}
		h.e.addWindow(w)
		h.at = h.at.Add(step)
	}
	return h
}

func BenchmarkBidDecision2048Windows(b *testing.B) {
	h := benchHistory(2048)
	b.ResetTimer()
	for range b.N {
		h.decide(route, 1_000_000)
	}
}

func BenchmarkBidDecisionAtWindowLimit(b *testing.B) {
	h := benchHistory(bidWindowLimit)
	b.ResetTimer()
	for range b.N {
		h.decide(route, 1_000_000)
	}
}
