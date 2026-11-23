package ingest

import (
	"context"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

// Reconcile installs a newly hash-verified standard pool base, compares cached
// Fast receipts against the complete standard receipt list, and replays only
// the still-unconfirmed suffix. It never publishes the Fast state as the base.
// Conflicts discard that suffix; the verified new base is retained for recovery.
// It returns the pools the replayed suffix changed: the rebase retires their
// search tickets, so they are searched again with the block's own pools.
func (b *FastBranch) Reconcile(ctx context.Context, base *state.Store, transactions []Transaction) ([]domain.PoolKey, error) {
	if err := ctx.Err(); err != nil {
		b.Reset()
		return nil, err
	}
	if base == nil {
		return nil, fmt.Errorf("%w: nil reconciliation base", ErrInvalidConfig)
	}
	previous := b.base.Block()
	ref := base.Block()
	records, waiting := b.records, b.waiting
	if err := b.ResetTo(base); err != nil {
		b.Reset()
		return nil, err
	}
	if ref.Number <= previous.Number || ref.Number-previous.Number != 1 || ref.ParentHash != previous.Hash {
		return nil, ErrFastContinuity
	}
	for i, tx := range transactions {
		if tx.Index != uint64(i) || tx.Hash == (domain.Hash{}) || (!tx.Successful && len(tx.Logs) != 0) {
			return nil, fmt.Errorf("%w: invalid canonical receipt index %d", ErrInvalidConfig, i)
		}
	}
	for _, record := range records {
		message := record.message
		if message.BlockNumber > ref.Number {
			continue
		}
		// Translate the feed's position using the explicit system-tx mapping.
		relative := message.TxIndex - b.opts.FirstTxIndex
		index := b.opts.CanonicalFirstTxIndex + relative
		if message.BlockNumber != ref.Number || index < relative || index >= uint64(len(transactions)) {
			return nil, ErrFastConflict
		}
		canonical := transactions[index]
		canonical.Index = message.TxIndex
		if transactionDigest(canonical) != record.digest {
			return nil, ErrFastConflict
		}
	}
	// System transactions before the mapped first position may not be omitted
	// if they change a tracked DEX pool. Such a mapping cannot support this view.
	for i := uint64(0); i < b.opts.CanonicalFirstTxIndex && i < uint64(len(transactions)); i++ {
		for _, log := range transactions[i].Logs {
			if _, tracked := b.routes.resolveKey(log); tracked {
				return nil, ErrFastConflict
			}
		}
	}
	var affected []domain.PoolKey
	for _, record := range records {
		if record.message.BlockNumber <= ref.Number {
			continue
		}
		changed, err := b.Apply(ctx, record.message)
		if err != nil {
			return nil, err
		}
		affected = append(affected, changed...)
	}
	// An empty branch resumes from its waiting run once the base is the
	// run's parent; a run further ahead keeps waiting.
	if len(records) == 0 {
		for len(waiting) > 0 && waiting[0].BlockNumber <= ref.Number {
			waiting = waiting[1:]
		}
		if len(waiting) > 0 && (waiting[0].BlockNumber != ref.Number+1 || waiting[0].TxIndex != b.opts.FirstTxIndex) {
			b.waiting = waiting
			waiting = nil
		}
		for _, message := range waiting {
			changed, err := b.Apply(ctx, message)
			if err != nil {
				return nil, err
			}
			affected = append(affected, changed...)
		}
	}
	slices.SortFunc(affected, domain.ComparePoolKeys)
	return slices.Compact(affected), nil
}
