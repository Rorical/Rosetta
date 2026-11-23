package arb

import (
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

const rankBatchSize = 2048

// rankPass is one progressive ranking of the routes of from to to pools; zero
// bounds mean every depth. Its batches take cores of class and fill buckets,
// which a job's short and long passes share so the last one selects over
// both. afterFirst, when set, runs after the first batch of each short depth.
type rankPass struct {
	from, to   int
	class      cpuClass
	buckets    map[domain.Address]*rankBucket
	afterFirst func() error
}

// Batches retain the same depth/pool reservoirs as full ranking. Trimming each
// batch to K would lose representatives needed by the final diverse selection.
func (w *Worker) progressiveSchedule(ctx context.Context, job *SearchJob, engine *Engine, cache *rankCache, lease *searchCPULease, preparedAt time.Time, pass rankPass) rankSchedule {
	limit := engine.K()
	if pass.from == 0 {
		pass.from = 2
	}
	if pass.to == 0 {
		pass.to = 16
	}
	return func(indices []int, workers int, rank func([]int, int) (RankedSelection, error)) (RankedSelection, error) {
		cache.batchIndices = slices.Grow(cache.batchIndices[:0], len(indices))[:len(indices)]
		groups := rankDepths(engine, indices, cache.batchIndices, workers)
		job.Stages.Rank += time.Since(preparedAt)
		if pass.class == cpuUrgent {
			// Keep a core for these batches across the gaps between leases.
			w.cpus.expect(1)
			defer w.cpus.met(1)
		}
		lease.release()
		totals := RankedSelection{}
		buckets := pass.buckets
		if buckets == nil {
			buckets = make(map[domain.Address]*rankBucket)
		}
		// Short routes publish before long variants of the same gap. Remaining
		// depths alternate batches; cursors advance even when ranking is canceled.
		var remaining [17]int
		for depth := pass.from; depth <= pass.to; depth++ {
			remaining[depth] = len(groups[depth])
		}
		// take reserves the next batch of depth as a serial visit would.
		cursor := cache.progressCursor
		take := func(depth int) (rankBatch, bool) {
			group := groups[depth]
			if remaining[depth] == 0 {
				return rankBatch{}, false
			}
			start := cursor[depth] % len(group)
			count := min(rankBatchSize, remaining[depth], len(group)-start)
			cursor[depth] = (start + count) % len(group)
			remaining[depth] -= count
			return rankBatch{depth: depth, paths: group[start : start+count], cursor: cursor[depth]}, true
		}
		commit := func(batch rankBatch, part RankedSelection) error {
			cache.progressCursor[batch.depth] = batch.cursor
			totals.considered += part.considered
			for _, issue := range part.issues {
				totals.issues = appendIssue(totals.issues, issue)
			}
			for _, candidate := range part.candidates {
				bucket := buckets[candidate.cycle.Start]
				if bucket == nil {
					bucket = &rankBucket{diverse: w.opts.Diverse}
					buckets[candidate.cycle.Start] = bucket
				}
				bucket.admit(candidate, limit, false)
			}
			return w.probeRankBatch(ctx, job, engine, part, batch.depth, limit)
		}
		// Give each short depth the first batch, then rotate every depth so a
		// large short-path population cannot starve the rest of the graph.
		for depth := pass.from; depth <= min(pass.to, shortRouteHops); depth++ {
			batch, ok := take(depth)
			if !ok {
				continue
			}
			waiting := time.Now()
			// A 2048-path batch has at most two useful ranking shards. Do not
			// reserve idle cores that the exact pipeline can use concurrently.
			wanted := min(workers, w.rankCapacity(job), max(1, (len(batch.paths)+1023)/1024))
			batchLease, err := w.cpus.acquireClass(ctx, wanted, pass.class)
			job.Stages.RankCPU += time.Since(waiting)
			if err != nil {
				return totals, err
			}
			started := time.Now()
			part, err := rankShardMembers(batch.paths, limit, batchLease.count, w.opts.Diverse, true, rank)
			batchLease.release()
			job.Stages.Rank += time.Since(started)
			if err == nil {
				err = commit(batch, part)
			}
			if err != nil {
				return totals, err
			}
		}
		if pass.afterFirst != nil {
			if err := pass.afterFirst(); err != nil {
				return totals, err
			}
		}
		var plan []rankBatch
		for more := true; more; {
			more = false
			for depth := pass.from; depth <= pass.to; depth++ {
				if batch, ok := take(depth); ok {
					plan = append(plan, batch)
					more = true
				}
			}
		}
		if err := w.rankAhead(ctx, job, plan, workers, limit, pass.class, rank, commit); err != nil {
			return totals, err
		}
		totals.candidates = append(totals.candidates, w.selectBuckets(buckets, limit)...)
		sortRanked(totals.candidates)
		return totals, ctx.Err()
	}
}

// rankBatch is one planned progressive batch and the depth cursor after it.
type rankBatch struct {
	depth, cursor int
	paths         []int
}

// rankAhead ranks planned batches on free cores ahead of their turn and
// commits them in plan order, so probes and admissions are those of ranking
// one batch at a time. One batch at a time leaves most cores idle on a large
// table. Each batch takes
// one CPU slot, so exact work queued by earlier commits gets slots between
// batches.
func (w *Worker) rankAhead(ctx context.Context, job *SearchJob, plan []rankBatch, workers, limit int, class cpuClass, rank func([]int, int) (RankedSelection, error), commit func(rankBatch, RankedSelection) error) error {
	if len(plan) == 0 {
		return ctx.Err()
	}
	rankers := max(1, min(workers, w.rankCapacity(job), len(plan)))
	if class == cpuUrgent {
		// Rankers return their core between batches; keep it from long routes.
		w.cpus.expect(rankers)
		defer w.cpus.met(rankers)
	}
	if rankers == 1 {
		// One core or one batch: rank on this goroutine as batches always did.
		// A ranker goroutine would queue behind the exact tasks that the short
		// batches just admitted, and deep routes would wait for their sizing.
		for _, batch := range plan {
			waiting := time.Now()
			wanted := min(workers, w.rankCapacity(job), max(1, (len(batch.paths)+1023)/1024))
			lease, err := w.cpus.acquireClass(ctx, wanted, class)
			job.Stages.RankCPU += time.Since(waiting)
			if err != nil {
				return err
			}
			started := time.Now()
			part, err := rankShardMembers(batch.paths, limit, lease.count, w.opts.Diverse, true, rank)
			lease.release()
			job.Stages.Rank += time.Since(started)
			if err == nil {
				err = commit(batch, part)
			}
			if err != nil {
				return err
			}
		}
		return ctx.Err()
	}
	type ranked struct {
		part RankedSelection
		err  error
		wait time.Duration
		done chan struct{}
	}
	results := make([]ranked, len(plan))
	for i := range results {
		results[i].done = make(chan struct{})
	}
	rankCtx, stop := context.WithCancel(ctx)
	var next atomic.Int64
	var group sync.WaitGroup
	for range rankers {
		group.Go(func() {
			for i := int(next.Add(1) - 1); i < len(plan); i = int(next.Add(1) - 1) {
				r := &results[i]
				waiting := time.Now()
				lease, err := w.cpus.acquireClass(rankCtx, 1, class)
				r.wait = time.Since(waiting)
				if err == nil {
					r.part, err = rankShardMembers(plan[i].paths, limit, 1, w.opts.Diverse, true, rank)
					lease.release()
				}
				r.err = err
				close(r.done)
			}
		})
	}
	var err error
	// Rank is wall time, as when batches ran one at a time; RankCPU sums the
	// rankers' waits for a CPU slot.
	committed := time.Now()
	for i := range plan {
		select {
		case <-results[i].done:
		case <-rankCtx.Done():
			err = rankCtx.Err()
		}
		if err != nil {
			break
		}
		r := &results[i]
		job.Stages.RankCPU += r.wait
		now := time.Now()
		job.Stages.Rank += now.Sub(committed)
		committed = now
		if err = r.err; err == nil {
			err = commit(plan[i], r.part)
		}
		r.part = RankedSelection{}
		if err != nil {
			break
		}
	}
	// No snapshot or workspace is released while a ranker still reads it.
	stop()
	group.Wait()
	return err
}

// parallelDepthIndices is the fewest indices worth grouping on several cores.
const parallelDepthIndices = 1 << 16

// rankDepths groups indices by route length, each group in index order. A
// compiled engine with workers > 1 counts and scatters chunks on all of them
// (serially, about 12% of search CPU on a large table).
func rankDepths(engine *Engine, indices, storage []int, workers int) [17][]int {
	if engine.compiled != nil && workers > 1 && len(indices) >= parallelDepthIndices {
		return compiledDepths(engine.compiled.paths, indices, storage, workers)
	}
	var counts [17]int
	var work *rankWorkspace
	if engine.compiled == nil {
		work = rankWorkspaces.Get().(*rankWorkspace)
		defer work.release()
	}
	depthOf := func(index int) int {
		if engine.compiled != nil {
			return min(16, int(engine.compiled.paths[index].Length))
		}
		return min(16, len(work.cycle(engine.table, index).Pools))
	}
	for _, index := range indices {
		counts[depthOf(index)]++
	}
	var groups [17][]int
	offset := 0
	for depth, count := range counts {
		groups[depth] = storage[offset : offset : offset+count]
		offset += count
	}
	for _, index := range indices {
		depth := depthOf(index)
		groups[depth] = append(groups[depth], index)
	}
	return groups
}

func compiledDepths(paths []graph.PackedPath, indices, storage []int, workers int) [17][]int {
	chunk := func(w int) []int { return indices[w*len(indices)/workers : (w+1)*len(indices)/workers] }
	counts := make([][17]int, workers)
	var group sync.WaitGroup
	for w := range workers {
		group.Go(func() {
			for _, index := range chunk(w) {
				counts[w][min(16, int(paths[index].Length))]++
			}
		})
	}
	group.Wait()
	// Chunk w writes depth d after every earlier chunk's routes of depth d.
	var groups [17][]int
	offset := 0
	for depth := range groups {
		start := offset
		for w := range workers {
			start, counts[w][depth] = start+counts[w][depth], start
		}
		groups[depth] = storage[offset:start:start]
		offset = start
	}
	for w := range workers {
		group.Go(func() {
			cursor := counts[w]
			for _, index := range chunk(w) {
				depth := min(16, int(paths[index].Length))
				storage[cursor[depth]] = index
				cursor[depth]++
			}
		})
	}
	group.Wait()
	return groups
}

// selectBuckets reduces each funding asset's reservoir to its selection, on the
// lane and any free cores. One asset after another holds the lane for about
// 1 ms after the last ranking batch.
func (w *Worker) selectBuckets(buckets map[domain.Address]*rankBucket, limit int) []rankedCycle {
	list := slices.Collect(maps.Values(buckets))
	parts := make([][]rankedCycle, len(list))
	var next atomic.Int64
	run := func() {
		for i := int(next.Add(1) - 1); i < len(list); i = int(next.Add(1) - 1) {
			parts[i] = list[i].selectCandidates(limit)
		}
	}
	var group sync.WaitGroup
	for range len(list) - 1 {
		lease, ok := w.cpus.tryAcquire()
		if !ok {
			break
		}
		group.Go(func() {
			defer lease.release()
			run()
		})
	}
	run()
	group.Wait()
	return slices.Concat(parts...)
}
