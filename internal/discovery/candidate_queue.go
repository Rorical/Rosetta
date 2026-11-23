package discovery

import (
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// CandidateQueue checks previously unseen markets before retrying earlier
// assessments. Rejected high-activity pools cannot occupy every round forever.
// The discovery worker owns it; reset it when changing canonical generations.
type CandidateQueue struct {
	round   uint64
	checked map[domain.PoolKey]uint64
}

func (q *CandidateQueue) Next(rows []Creation, activity map[domain.PoolKey]Activity, limit int) []Creation {
	if limit <= 0 {
		return nil
	}
	if q.checked == nil {
		q.checked = make(map[domain.PoolKey]uint64)
	}
	present := make(map[domain.PoolKey]bool, len(rows))
	for _, row := range rows {
		present[row.Spec.PoolKey()] = true
	}
	for key := range q.checked {
		if !present[key] {
			delete(q.checked, key)
		}
	}
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b Creation) int {
		ak, bk := a.Spec.PoolKey(), b.Spec.PoolKey()
		if q.checked[ak] < q.checked[bk] {
			return -1
		}
		if q.checked[ak] > q.checked[bk] {
			return 1
		}
		aStats, bStats := ActivityFor(activity, ak), ActivityFor(activity, bk)
		if aStats.MultiSwapUses > bStats.MultiSwapUses {
			return -1
		}
		if aStats.MultiSwapUses < bStats.MultiSwapUses {
			return 1
		}
		as, bs := aStats.Swaps, bStats.Swaps
		if as > bs {
			return -1
		}
		if as < bs {
			return 1
		}
		return domain.ComparePoolKeys(ak, bk)
	})
	q.round++
	rows = rows[:min(limit, len(rows))]
	for _, row := range rows {
		q.checked[row.Spec.PoolKey()] = q.round
	}
	return rows
}

// ProbeAllowance reserves a quarter of the round for old active emitters, even
// when indexed creations fill the shortlist. Unused indexed slots can also probe.
func ProbeAllowance(limit, indexed int) int {
	return limit - min(indexed, limit-max(1, limit/4))
}
