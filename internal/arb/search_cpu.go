package arb

import (
	"context"
	"runtime"
	"slices"
	"sync"
)

// shortRouteHops is the longest route whose exact work runs ahead of longer
// routes and of ranking. Two- and three-hop routes carry most arbitrage value;
// without this their sizing waits behind four-hop ranking and sizing until the
// end of each frame.
const shortRouteHops = 3

// cpuClass orders waiters for a freed core, each class FIFO: urgent, then
// backlog, then normal. Urgent is the short-route work of a source's newest
// search, backlog the short-route sizing of its earlier ones, normal
// everything else. Without the backlog class the selections of up to three
// searches sized side by side: in a synthetic 150-frame burst with -cpu 7 a
// frame's first short quote came after 13.6 to 13.8 ms (p50) instead of 11.3
// to 11.9 ms, with 2,650 to 2,790 short quotes a block instead of 3,050 to
// 3,220 (BenchmarkBurstBlock, three runs of ten blocks each).
type cpuClass int

const (
	cpuUrgent cpuClass = iota
	cpuBacklog
	cpuNormal
	cpuClasses
)

// routeClass is the CPU class of exact work on a route of the given pools.
func routeClass(pools int) cpuClass {
	if pools <= shortRouteHops {
		return cpuUrgent
	}
	return cpuNormal
}

// reservedShortCapacity is the fewest cores from which one is kept from the
// normal class. A search's long-route phase overlaps the next search's short
// routes (searchRun), and ranking holds a core for a whole 2048-route batch,
// about 1.5 ms on a large table. On fewer cores the reserve would cost long
// routes a third or more of the host.
const reservedShortCapacity = 4

// Both source lanes share this budget. Idle capacity can be borrowed by one
// lane; blocked publication does not reserve CPU slots or delay the other lane.
type searchCPUs struct {
	mu       sync.Mutex
	capacity int
	used     int
	// normal counts the cores held by the normal class, at most normalLimit.
	normal, normalLimit int
	// expected counts queued urgent work that has no core yet (expect).
	expected int
	waiting  [cpuClasses][]chan struct{}
}

type searchCPULease struct {
	cpus  *searchCPUs
	count int
	class cpuClass
}

func newSearchCPUs() *searchCPUs {
	return newSearchCPUsOf(runtime.GOMAXPROCS(0))
}

func newSearchCPUsOf(capacity int) *searchCPUs {
	capacity = max(1, capacity)
	limit := capacity
	if capacity >= reservedShortCapacity {
		limit = capacity - 1
	}
	return &searchCPUs{capacity: capacity, normalLimit: limit}
}

// free reports whether class may take one more core. The caller holds mu.
func (c *searchCPUs) free(class cpuClass) bool {
	if c.used >= c.capacity {
		return false
	}
	return class != cpuNormal || (c.normal < c.normalLimit && c.capacity-c.used > c.expected)
}

// waiters counts the waiters of class and of every class ahead of it. The
// caller holds mu.
func (c *searchCPUs) waiters(class cpuClass) int {
	count := 0
	for ahead := cpuUrgent; ahead <= class; ahead++ {
		count += len(c.waiting[ahead])
	}
	return count
}

// expect registers n pieces of queued urgent work that will each ask for a
// core, and keeps that many free cores from the normal class until met. The
// waiting queues alone order only those already blocked: a short route's
// sizing gives its core back after every eight amounts, and long-route ranking
// waiting at that instant takes it for a whole batch, so short-route sizing
// can stretch from 2.7 ms to 33 ms into a 39 ms frame.
func (c *searchCPUs) expect(n int) {
	c.mu.Lock()
	c.expected += n
	c.mu.Unlock()
}

// met ends n expectations: the work holds its core or no longer wants one.
func (c *searchCPUs) met(n int) {
	c.mu.Lock()
	c.expected -= n
	c.grant()
	c.mu.Unlock()
}

// take accounts one more core to class. The caller holds mu.
func (c *searchCPUs) take(class cpuClass) {
	c.used++
	if class == cpuNormal {
		c.normal++
	}
}

// give returns count cores of class. The caller holds mu.
func (c *searchCPUs) give(class cpuClass, count int) {
	c.used -= count
	if class == cpuNormal {
		c.normal -= count
	}
}

// acquire waits for one normal-class core, then takes up to wanted in all
// while they are free and nobody waits.
func (c *searchCPUs) acquire(ctx context.Context, wanted int) (searchCPULease, error) {
	return c.acquireClass(ctx, wanted, cpuNormal)
}

func (c *searchCPUs) acquireClass(ctx context.Context, wanted int, class cpuClass) (searchCPULease, error) {
	lease := searchCPULease{cpus: c, class: class}
	c.mu.Lock()
	if c.free(class) && c.waiters(class) == 0 {
		c.take(class)
	} else {
		granted := make(chan struct{})
		c.waiting[class] = append(c.waiting[class], granted)
		c.mu.Unlock()
		select {
		case <-granted:
		case <-ctx.Done():
			c.mu.Lock()
			if i := slices.Index(c.waiting[class], granted); i >= 0 {
				c.waiting[class] = slices.Delete(c.waiting[class], i, i+1)
				c.mu.Unlock()
				return lease, ctx.Err()
			}
			// Granted while canceling: hand the core on.
			c.give(class, 1)
			c.grant()
			c.mu.Unlock()
			return lease, ctx.Err()
		}
		c.mu.Lock()
	}
	lease.count = 1
	for lease.count < min(wanted, c.capacity) && c.free(class) && c.waiters(cpuNormal) == 0 {
		c.take(class)
		lease.count++
	}
	c.mu.Unlock()
	return lease, nil
}

// tryAcquire takes one normal-class core only if it is free and nobody waits
// for it.
func (c *searchCPUs) tryAcquire() (searchCPULease, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.free(cpuNormal) || c.waiters(cpuNormal) > 0 {
		return searchCPULease{}, false
	}
	c.take(cpuNormal)
	return searchCPULease{cpus: c, count: 1, class: cpuNormal}, true
}

// held is the number of cores currently leased.
func (c *searchCPUs) held() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// grant hands free cores to waiters in class order. The caller holds mu.
func (c *searchCPUs) grant() {
	for c.used < c.capacity {
		class := cpuUrgent
		for class < cpuNormal && len(c.waiting[class]) == 0 {
			class++
		}
		if len(c.waiting[class]) == 0 || !c.free(class) {
			return
		}
		granted := c.waiting[class][0]
		c.waiting[class] = slices.Delete(c.waiting[class], 0, 1)
		c.take(class)
		close(granted)
	}
}

func (l *searchCPULease) release() {
	if l.count == 0 {
		return
	}
	c := l.cpus
	c.mu.Lock()
	c.give(l.class, l.count)
	l.count = 0
	c.grant()
	c.mu.Unlock()
}
