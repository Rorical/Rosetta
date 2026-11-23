package pipeline

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"slices"
)

func (b *paperBook) clear() {
	clear(b.headers)
	clear(b.quotes)
	b.paid = nil
	b.head = domain.BlockRef{}
}

func (b *paperBook) truncate(number uint64) {
	for n := range b.headers {
		if n > number {
			delete(b.headers, n)
		}
	}
	b.paid = slices.DeleteFunc(b.paid, func(s paidSample) bool { return s.block.Number > number })
	for key, samples := range b.quotes {
		samples = slices.DeleteFunc(samples, func(s quoteSample) bool { return s.block > number })
		if len(samples) == 0 {
			delete(b.quotes, key)
		} else {
			b.quotes[key] = samples
		}
	}
}

func (b *paperBook) prune() {
	floor := func(history uint64) uint64 {
		if b.head.Number > history {
			return b.head.Number - history
		}
		return 0
	}
	quoteHistory := b.policy.QuoteHistoryBlocks
	if quoteHistory == 0 {
		quoteHistory = b.policy.HistoryBlocks
	}
	headerFloor := floor(max(quoteHistory, b.policy.HistoryBlocks))
	for n := range b.headers {
		if n < headerFloor {
			delete(b.headers, n)
		}
	}
	feeFloor, quoteFloor := floor(b.policy.HistoryBlocks), floor(quoteHistory)
	b.paid = slices.DeleteFunc(b.paid, func(s paidSample) bool { return s.block.Number < feeFloor })
	for key, samples := range b.quotes {
		samples = slices.DeleteFunc(samples, func(s quoteSample) bool { return s.block < quoteFloor })
		if len(samples) == 0 {
			delete(b.quotes, key)
		} else {
			b.quotes[key] = samples
		}
	}
}

// Bound disk and heap independently of market churn. Evict the least recently
// observed direction as a unit; its next quote becomes explicitly uncalibrated.
func (b *paperBook) limitQuotes() {
	count := 0
	for _, samples := range b.quotes {
		count += len(samples)
	}
	for len(b.quotes) > 4096 || count > 65536 {
		var oldest quoteDirection
		oldestBlock := ^uint64(0)
		for key, samples := range b.quotes {
			latest := uint64(0)
			for _, sample := range samples {
				latest = max(latest, sample.block)
			}
			if latest < oldestBlock || latest == oldestBlock && compareQuoteDirections(key, oldest) < 0 {
				oldest, oldestBlock = key, latest
			}
		}
		count -= len(b.quotes[oldest])
		delete(b.quotes, oldest)
	}
}
func compareQuoteDirections(a, b quoteDirection) int {
	if order := domain.ComparePoolKeys(a.Pool, b.Pool); order != 0 {
		return order
	}
	for i := range a.TokenIn {
		if a.TokenIn[i] < b.TokenIn[i] {
			return -1
		}
		if a.TokenIn[i] > b.TokenIn[i] {
			return 1
		}
	}
	return 0
}
