package graph

import (
	"math/bits"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
)

// CandidateBuffer owns reusable bitmap/slice storage for one caller. Select's
// result is sorted and remains valid only until the next Select on this buffer.
type CandidateBuffer struct {
	marks []uint64
	ids   []int
}

func (b *CandidateBuffer) Select(table CycleSource, dirty []domain.PoolKey, k int) []int {
	b.ids = b.ids[:0]
	count := table.Len()
	words := (count + 63) / 64
	b.marks = slices.Grow(b.marks[:0], words)[:words]
	clear(b.marks)
	if len(dirty) == 0 || k <= 0 {
		return nil
	}
	visitCandidates(table, dirty, k, func(index int) {
		if index >= 0 && index < count {
			b.marks[index/64] |= uint64(1) << uint(index%64)
		}
	})
	for word, marked := range b.marks {
		for marked != 0 {
			bit := bits.TrailingZeros64(marked)
			b.ids = append(b.ids, word*64+bit)
			marked &= marked - 1
		}
	}
	if len(b.ids) == 0 {
		return nil
	}
	return b.ids
}

// parallelSelectPostings is the fewest dirty-pool postings worth spreading
// over workers; below it the serial walk finishes before they would start.
const parallelSelectPostings = 1 << 16

// SelectWorkers is Select with the postings walk and the sorted extraction of
// a packed table spread over workers; the result is the same. On a complete
// table of about 9M routes the serial walk takes about a third of the search
// CPU before the first short route ranks.
func (b *CandidateBuffer) SelectWorkers(table CycleSource, dirty []domain.PoolKey, k, workers int) []int {
	return b.selectWorkers(table, dirty, k, workers, parallelSelectPostings)
}

func (b *CandidateBuffer) selectWorkers(table CycleSource, dirty []domain.PoolKey, k, workers, minimum int) []int {
	packed, ok := table.(*Table)
	if !ok || packed.packed == nil || workers < 2 || k <= 0 {
		return b.Select(table, dirty, k)
	}
	layout := packed.packed
	var spans [][]uint32
	total := 0
	for _, key := range dirty {
		id, ok := layout.poolIDs[key]
		if !ok {
			continue
		}
		postings := layout.postings[layout.offsets[id]:layout.offsets[id+1]]
		postings = postings[:min(k, len(postings))]
		spans = append(spans, postings)
		total += len(postings)
	}
	if total < minimum || total == 0 {
		return b.Select(table, dirty, k)
	}
	words := (table.Len() + 63) / 64
	b.marks = slices.Grow(b.marks[:0], words)[:words]
	clear(b.marks)
	// Each worker marks an equal share of the postings laid end to end.
	var group sync.WaitGroup
	for w := range workers {
		from, to := w*total/workers, (w+1)*total/workers
		group.Go(func() {
			offset := 0
			for _, span := range spans {
				if lo, hi := max(from-offset, 0), min(to-offset, len(span)); lo < hi {
					for _, index := range span[lo:hi] {
						atomic.OrUint64(&b.marks[index/64], uint64(1)<<(index%64))
					}
				}
				if offset += len(span); offset >= to {
					return
				}
			}
		})
	}
	group.Wait()
	// Each worker then extracts a word range; prefix sums keep index order.
	counts := make([]int, workers+1)
	for w := range workers {
		group.Go(func() {
			for _, marked := range b.marks[w*words/workers : (w+1)*words/workers] {
				counts[w+1] += bits.OnesCount64(marked)
			}
		})
	}
	group.Wait()
	for w := range workers {
		counts[w+1] += counts[w]
	}
	b.ids = slices.Grow(b.ids[:0], counts[workers])[:counts[workers]]
	for w := range workers {
		group.Go(func() {
			first := w * words / workers
			out := b.ids[counts[w]:counts[w]]
			for word, marked := range b.marks[first : (w+1)*words/workers] {
				for marked != 0 {
					out = append(out, (first+word)*64+bits.TrailingZeros64(marked))
					marked &= marked - 1
				}
			}
		})
	}
	group.Wait()
	if len(b.ids) == 0 {
		return nil
	}
	return b.ids
}

func visitCandidates(table CycleSource, dirty []domain.PoolKey, k int, visit func(int)) {
	if indexed, ok := table.(interface {
		visitCandidates([]domain.PoolKey, int, func(int))
	}); ok {
		indexed.visitCandidates(dirty, k, visit)
		return
	}
	for _, index := range table.Candidates(dirty, k) {
		visit(index)
	}
}

func visitPostings(postings map[domain.PoolKey][]int, dirty []domain.PoolKey, k int, visit func(int)) {
	for _, key := range dirty {
		indices := postings[key]
		for _, index := range indices[:min(k, len(indices))] {
			visit(index)
		}
	}
}

func (t *Table) visitCandidates(dirty []domain.PoolKey, k int, visit func(int)) {
	if t.packed != nil {
		t.packed.visit(dirty, k, visit)
		return
	}
	visitPostings(t.byPool, dirty, k, visit)
}

func (t *DeepTable) visitCandidates(dirty []domain.PoolKey, k int, visit func(int)) {
	visitPostings(t.byPool, dirty, k, visit)
}

func (t *MergedTable) visitCandidates(dirty []domain.PoolKey, k int, visit func(int)) {
	for i, source := range t.sources {
		visitCandidates(source, dirty, k, func(index int) {
			if index >= 0 && index < len(t.remap[i]) && t.remap[i][index] >= 0 {
				visit(t.remap[i][index])
			}
		})
	}
}
