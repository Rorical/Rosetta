package discovery

import (
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

type ActivityRecord struct {
	Key            domain.PoolKey
	Counts         []BlockCount
	Unknown        bool
	LastBlock      uint64
	Span           uint64
	ProfitableUses uint64
	MultiSwapUses  uint64
}
type BlockCount struct{ Number, Count uint64 }

func (b *ActivityBook) Records() []ActivityRecord {
	out := []ActivityRecord{}
	for key, buckets := range b.buckets {
		row := ActivityRecord{Key: key, ProfitableUses: b.profitable[key], MultiSwapUses: b.multiSwap[key], Unknown: b.unknown[key], LastBlock: b.last[key], Span: b.span}
		for number, count := range buckets {
			row.Counts = append(row.Counts, BlockCount{number, count})
		}
		slices.SortFunc(row.Counts, func(a, c BlockCount) int {
			if a.Number < c.Number {
				return -1
			}
			if a.Number > c.Number {
				return 1
			}
			return 0
		})
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, c ActivityRecord) int { return domain.ComparePoolKeys(a.Key, c.Key) })
	return out
}
func (b *ActivityBook) Restore(records []ActivityRecord, anchor domain.BlockRef) error {
	if len(records) > 8192 {
		return fmt.Errorf("activity index exceeds pool limit")
	}
	restored := ActivityBook{observed: anchor, buckets: map[domain.PoolKey]map[uint64]uint64{}, profitable: map[domain.PoolKey]uint64{}, multiSwap: map[domain.PoolKey]uint64{}, unknown: map[domain.PoolKey]bool{}, last: map[domain.PoolKey]uint64{}}
	for _, row := range records {
		if len(row.Counts) > 130 {
			return fmt.Errorf("activity index exceeds block limit")
		}
		if row.LastBlock > anchor.Number || row.Span == 0 || restored.span != 0 && restored.span != row.Span {
			return fmt.Errorf("invalid activity time bins")
		}
		if _, exists := restored.buckets[row.Key]; exists {
			return fmt.Errorf("duplicate activity key")
		}
		restored.span = row.Span
		restored.last[row.Key] = row.LastBlock
		if row.Unknown {
			restored.unknown[row.Key] = true
		}
		restored.buckets[row.Key] = map[uint64]uint64{}
		for _, bucket := range row.Counts {
			if bucket.Number > anchor.Number {
				return fmt.Errorf("activity exceeds index anchor")
			}
			restored.buckets[row.Key][bucket.Number] = bucket.Count
		}
		restored.profitable[row.Key] = row.ProfitableUses
		restored.multiSwap[row.Key] = row.MultiSwapUses
	}
	*b = restored
	return nil
}
