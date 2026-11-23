package arb

import (
	"math/big"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func queueTask(asset byte, depth int, estimated, observed int64) *exactTask {
	return &exactTask{candidate: rankedCycle{cycle: graph.Cycle{Start: domain.Address{asset}, Pools: make([]domain.PoolKey, depth), Len: depth}, net: big.NewInt(estimated)}, search: &amountSearch{quoted: true, best: Opportunity{Net: big.NewInt(observed)}}}
}

func TestExactQueuePrioritizesObservedProfitWithinRotatingAssets(t *testing.T) {
	low := queueTask(1, 3, 100000, -1)
	high := queueTask(1, 3, 1, 10)
	long := queueTask(1, 6, 1, 2)
	other := queueTask(2, 3, 1000000000, 1000000000)
	var q exactQueue
	for _, task := range []*exactTask{low, other, long, high} {
		q.push(task)
	}
	cursor := exactGroup{}
	for _, want := range []*exactTask{high, other, long, low} {
		got := q.popRanked(cursor)
		if got != want {
			t.Fatal("mixed currencies or prioritized losing estimate")
		}
		cursor = taskGroup(got)
	}
	if q.count != 0 {
		t.Fatal("lost queue removal")
	}
}

func TestExactQueueGivesLosingCandidatesBoundedFIFOTurn(t *testing.T) {
	low := queueTask(1, 3, 10000, -1)
	high := queueTask(1, 3, 1, 10)
	p := &exactPipeline{}
	p.ready = sync.NewCond(&p.mu)
	p.pending.push(low)
	p.pending.push(high)
	for i := range 5 {
		task := p.next()
		if i < 4 && task != high {
			t.Fatal("positive leader did not resume")
		}
		if i == 4 && task != low {
			t.Fatal("losing probe was starved")
		}
		p.pending.push(task)
	}
}

func TestExactQueuePrioritizesLargerObservedProfitOverSizingProgress(t *testing.T) {
	first, newer := queueTask(1, 3, 1, 10), queueTask(1, 6, 100, 100)
	first.search.work = &sizingWorkspace{count: 16}
	newer.search.work = &sizingWorkspace{count: 1}
	var q exactQueue
	q.push(newer)
	q.push(first)
	if q.popRanked(exactGroup{}) != newer {
		t.Fatal("low profit progress delayed a larger observed opportunity")
	}
	if q.pop() != first {
		t.Fatal("new route lost its FIFO turn")
	}
}
