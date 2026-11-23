package pipeline

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
)

type candidatePublications struct {
	version domain.StateVersion
	graph   uint64
	seen    map[[32]byte]struct{}
}

// A carried result and a queued recheck can both complete at the same prefix.
// Publish that exact quote once, while allowing changed sizes and leg outputs.
func (s *SearchScheduler) freshCandidate(event arb.SearchEvent) bool {
	index := 0
	if event.Job.Version.Source == domain.SourceFast {
		index = 1
	}
	cache := &s.publications[index]
	if cache.version != event.Job.Version || cache.graph != event.Job.GraphVersion {
		cache.version, cache.graph = event.Job.Version, event.Job.GraphVersion
		clear(cache.seen)
	}
	if cache.seen == nil {
		cache.seen = make(map[[32]byte]struct{})
	}
	quote := event.Result.Opportunities[0] // Partial events contain one candidate.
	h := sha256.New()
	h.Write([]byte(event.Stage)) // Optimized confirmation survives an identical initial quote.
	h.Write(quote.Cycle.Start[:])
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(quote.Cycle.Pools)))
	h.Write(count[:])
	for _, key := range quote.Cycle.Pools {
		h.Write(key.Address[:])
		h.Write([]byte{byte(key.Kind)})
		h.Write(key.PoolID[:])
		h.Write(key.Hooks[:])
	}
	writeQuoteAmount(h, quote.Input)
	writeQuoteAmount(h, quote.Output)
	for _, leg := range quote.Legs {
		writeQuoteAmount(h, leg.AmountIn)
		writeQuoteAmount(h, leg.AmountOut)
	}
	var key [32]byte
	h.Sum(key[:0])
	if _, duplicate := cache.seen[key]; duplicate {
		return false
	}
	// Above the bound, retain delivery instead of suppressing unknown quotes.
	if len(cache.seen) < 512 {
		cache.seen[key] = struct{}{}
	}
	return true
}

func writeQuoteAmount(h hash.Hash, amount *big.Int) {
	words := amount.Bytes()
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(words)))
	h.Write(length[:])
	h.Write(words)
}
