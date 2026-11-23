package ingest

import (
	"context"
	"maps"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ApplyBatch publishes a drained receipt suffix atomically. Forking the store
// preserves outstanding search views without replaying previously applied logs.
func (b *LogBranch) ApplyBatch(ctx context.Context, transactions []Transaction) ([]domain.PoolKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(transactions) == 0 {
		return nil, nil
	}
	if b.working == nil || len(b.blocks) == 0 {
		return nil, ErrFastContinuity
	}
	stage := *b
	stage.working = b.working.Fork()
	stage.affected = maps.Clone(b.affected)
	stage.blocks = slices.Clone(b.blocks)
	last := len(stage.blocks) - 1
	stage.blocks[last].transactions = slices.Clone(stage.blocks[last].transactions)
	dirty := make(map[domain.PoolKey]struct{})
	for _, transaction := range transactions {
		changed, err := stage.Apply(ctx, transaction)
		if err != nil {
			return nil, err
		}
		for _, key := range changed {
			dirty[key] = struct{}{}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	*b = stage
	keys := make([]domain.PoolKey, 0, len(dirty))
	for key := range dirty {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return keys, nil
}
