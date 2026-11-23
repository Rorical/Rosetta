package ingest

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

// LogObserver sees the state immediately before one tracked log. ReplayLogs
// owns its writer; the observer receives an immutable view and cannot alter it.
type LogObserver func(context.Context, domain.Hash, int, Log, domain.PoolKey, state.View) error

// ReplayLogs reuses the live reducers on a private fork for receipt/quote
// comparisons. It never publishes pool state or retains an execution reader.
func ReplayLogs(ctx context.Context, base state.View, ref domain.BlockRef, transactions []Transaction, observe LogObserver) error {
	branch, err := NewTransactionBranch(base.Fork())
	if err != nil {
		return err
	}
	if err := branch.Begin(ref); err != nil {
		return err
	}
	for index, tx := range transactions {
		if tx.Index != uint64(index) || tx.Hash == (domain.Hash{}) {
			return fmt.Errorf("receipt replay requires contiguous transaction identities")
		}
		if _, err := applyTransactionObserved(ctx, branch.working, branch.routing, ref, tx, observe); err != nil {
			return fmt.Errorf("receipt replay transaction %d: %w", index, err)
		}
	}
	return nil
}
