package arb

import (
	"bytes"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Reuse queue slots across amount quanta instead of allocating a new slice tail
// every time an unfinished task rejoins the queue.
type exactQueue struct {
	tasks       []*exactTask
	head, count int
}

func (q *exactQueue) push(task *exactTask) {
	if q.count == len(q.tasks) {
		grown := make([]*exactTask, max(8, 2*len(q.tasks)))
		for i := range q.count {
			grown[i] = q.tasks[(q.head+i)%len(q.tasks)]
		}
		q.tasks = grown
		q.head = 0
	}
	q.tasks[(q.head+q.count)%len(q.tasks)] = task
	q.count++
}
func (q *exactQueue) pop() *exactTask {
	task := q.tasks[q.head]
	q.tasks[q.head] = nil
	q.head = (q.head + 1) % len(q.tasks)
	q.count--
	return task
}

// Rotate funding assets and size larger observed profits before low-profit
// work with more accumulated progress.
// FIFO turns still advance other paths, including negative initial probes.
func (q *exactQueue) popRanked(after exactGroup) *exactTask {
	best := 0
	for i := 1; i < q.count; i++ {
		a, b := q.tasks[(q.head+i)%len(q.tasks)], q.tasks[(q.head+best)%len(q.tasks)]
		ga, gb := taskGroup(a), taskGroup(b)
		if groupOrder(ga, gb, after) < 0 || (ga == gb && exactTaskBetter(a, b)) {
			best = i
		}
	}
	task := q.tasks[(q.head+best)%len(q.tasks)]
	for i := best; i > 0; i-- {
		q.tasks[(q.head+i)%len(q.tasks)] = q.tasks[(q.head+i-1)%len(q.tasks)]
	}
	q.tasks[q.head] = task
	return q.pop()
}

type exactGroup struct {
	asset domain.Address
}

func taskGroup(t *exactTask) exactGroup {
	return exactGroup{t.candidate.cycle.Start}
}
func compareGroup(a, b exactGroup) int { return bytes.Compare(a.asset[:], b.asset[:]) }

func groupOrder(a, b, after exactGroup) int {
	aNext, bNext := compareGroup(a, after) > 0, compareGroup(b, after) > 0
	if aNext != bNext {
		if aNext {
			return -1
		}
		return 1
	}
	return compareGroup(a, b)
}
func exactTaskBetter(a, b *exactTask) bool {
	positive := func(t *exactTask) bool { return t.search != nil && t.search.quoted && t.search.best.Net.Sign() > 0 }
	ap, bp := positive(a), positive(b)
	if ap != bp {
		return ap
	}
	if ap {
		if order := a.search.best.Net.Cmp(b.search.best.Net); order != 0 {
			return order > 0
		}

		progress := func(t *exactTask) int {
			if t.search.work == nil {
				return 0
			}
			return t.search.work.count
		}
		if pa, pb := progress(a), progress(b); pa != pb {
			return pa > pb
		}
	}
	return compareRanked(a.candidate, b.candidate) < 0
}
