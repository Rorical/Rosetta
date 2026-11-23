package arb

import (
	"bytes"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
)

func rankInShards(indices []int, limit, workers int, diverse bool, rank func([]int, int) (RankedSelection, error)) (RankedSelection, error) {
	return rankShardMembers(indices, limit, workers, diverse, false, rank)
}

func rankShardMembers(indices []int, limit, workers int, diverse, retain bool, rank func([]int, int) (RankedSelection, error)) (RankedSelection, error) {
	parts := make([]RankedSelection, workers)
	failures := make([]error, workers)
	var group sync.WaitGroup
	for shard := range workers {
		run := func() {
			start, end := len(indices)*shard/workers, len(indices)*(shard+1)/workers
			parts[shard], failures[shard] = rank(indices[start:end], limit)
		}
		if workers == 1 {
			run()
		} else {
			group.Go(run)
		}
	}
	group.Wait() // No snapshot or workspace is released while a shard still reads it.
	selection := RankedSelection{}
	ranked := make(map[domain.Address]*rankBucket)
	for shard, part := range parts {
		if failures[shard] != nil {
			return selection, failures[shard]
		}
		selection.considered += part.considered
		for _, issue := range part.issues {
			selection.issues = appendIssue(selection.issues, issue)
		}
		for _, candidate := range part.candidates {
			bucket := ranked[candidate.cycle.Start]
			if bucket == nil {
				bucket = &rankBucket{diverse: diverse}
				ranked[candidate.cycle.Start] = bucket
			}
			bucket.admit(candidate, limit, false)
		}
	}
	for _, bucket := range ranked {
		if retain {
			selection.candidates = append(selection.candidates, bucket.members()...)
		} else {
			selection.candidates = append(selection.candidates, bucket.selectCandidates(limit)...)
		}
	}
	sortRanked(selection.candidates)
	return selection, nil
}

func sortRanked(candidates []rankedCycle) {
	slices.SortFunc(candidates, func(a, b rankedCycle) int {
		if order := bytes.Compare(a.cycle.Start[:], b.cycle.Start[:]); order != 0 {
			return order
		}
		return compareRanked(a, b)
	})
}
