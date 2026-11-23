package ingest

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

// Log is one EVM log from a block receipt.
type Log struct {
	Address domain.Address
	Topics  []domain.Hash
	Data    []byte
}

// Transaction contains only final receipt logs. Failed EVM transactions still
// occupy an index, but must not carry effects that execution rolled back.
type Transaction struct {
	Fees       *domain.ReceiptFees `json:"fees,omitempty"`
	Hash       domain.Hash
	From       *domain.Address `json:"from,omitempty"` // canonical receipts only
	Index      uint64
	Successful bool
	Logs       []Log
	// Raw is the signed transaction when the source carries it (the feeds);
	// a block read as receipts has none. Only state that is priced by
	// calldata reads it (MetricOmm oracle updates).
	Raw []byte `json:"-"`
}

// TransactionBranch stages a standard-feed block over an accepted pool store.
// Apply returns the dirty pools of one complete transaction. Commit publishes
// the whole block only after the caller has verified its produced hash. It is
// single-writer; neither Base nor Working may be mutated by callers.
type TransactionBranch struct {
	base         *state.Store
	working      *state.Store
	routing      routing
	ref          domain.BlockRef
	nextIndex    uint64
	transactions []domain.Hash
}

// NewTransactionBranch registers the pools in an accepted snapshot. It does
// not mutate that snapshot or issue RPC calls.
func NewTransactionBranch(base *state.Store) (*TransactionBranch, error) {
	if base == nil {
		return nil, fmt.Errorf("%w: nil transaction branch base", ErrInvalidConfig)
	}
	keys := make([]domain.PoolKey, 0)
	for _, p := range base.Pools() {
		keys = append(keys, p.Key)
	}
	routes, err := newRouting(keys)
	if err != nil {
		return nil, err
	}
	return &TransactionBranch{base: base, routing: routes}, nil
}

// Begin refuses to skip a block or replace an unfinished attempt. A new fork
// shares immutable pool values until writes replace them; failure is a discard.
func (b *TransactionBranch) Begin(ref domain.BlockRef) error {
	if b.working != nil {
		return fmt.Errorf("%w: pool block already open", ErrInvalidConfig)
	}
	head := b.base.Block()
	if ref.Hash == (domain.Hash{}) || ref.Number <= head.Number || ref.Number-head.Number != 1 || ref.ParentHash != head.Hash {
		return fmt.Errorf("%w: pool block %d does not extend head %d", ErrReorg, ref.Number, head.Number)
	}
	working := b.base.Fork()
	working.ClearDirty()
	b.working = working
	b.ref = ref
	b.nextIndex = 0
	b.transactions = nil
	return nil
}

// Apply opens one undo frame for all logs. A bad later log rolls back the
// complete transaction and leaves its index available for a corrected retry.
func (b *TransactionBranch) Apply(ctx context.Context, tx Transaction) ([]domain.PoolKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.working == nil || tx.Hash == (domain.Hash{}) || tx.Index != b.nextIndex {
		return nil, fmt.Errorf("%w: transaction index %d, want %d in an open block", ErrInvalidConfig, tx.Index, b.nextIndex)
	}
	dirty, err := applyTransaction(ctx, b.working, b.routing, b.ref, tx)
	if err != nil {
		return nil, err
	}
	b.transactions = append(b.transactions, tx.Hash)
	b.nextIndex++
	return dirty, nil
}

func applyTransaction(ctx context.Context, store *state.Store, routes routing, ref domain.BlockRef, tx Transaction) ([]domain.PoolKey, error) {
	return applyTransactionObserved(ctx, store, routes, ref, tx, nil)
}

func applyTransactionObserved(ctx context.Context, store *state.Store, routes routing, ref domain.BlockRef, tx Transaction, observe LogObserver) ([]domain.PoolKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !tx.Successful && len(tx.Logs) > 0 {
		return nil, fmt.Errorf("%w: failed transaction has logs", ErrInvalidConfig)
	}
	if err := store.BeginBlock(ref); err != nil {
		return nil, err
	}
	for i, log := range tx.Logs {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, store.Rollback())
		}
		store.ApplySlipstreamConfiguration(log.Address, log.Topics)
		if len(log.Topics) != 0 && log.Topics[0] == metricomm.TopicOracleUpdated && (i == 0 || tx.Logs[i-1].Address != log.Address) {
			applyMetricOracle(store, log.Address, tx, ref)
		}
		key, tracked := routes.resolveKey(log)
		if !tracked {
			continue
		}
		if observe != nil {
			if err := observe(ctx, tx.Hash, i, log, key, store.View()); err != nil {
				return nil, errors.Join(err, store.Rollback())
			}
		}
		if err := applyLog(store, log, key, ref.Number); err != nil {
			if rollbackErr := store.Rollback(); rollbackErr != nil {
				return nil, fmt.Errorf("apply transaction log %d: %w; rollback: %w", i, err, rollbackErr)
			}
			return nil, fmt.Errorf("apply transaction log %d: %w", i, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, store.Rollback())
	}
	dirty := store.Dirty()
	store.Commit()
	store.ClearDirty()
	return dirty, nil
}

// Commit must receive the final header metadata and the ordered receipt hashes
// from the verified block. Missing hook events cannot silently drop pool logs.
func (b *TransactionBranch) Commit(ref domain.BlockRef, hashes []domain.Hash) error {
	if err := b.ValidateCommit(ref, hashes); err != nil {
		return err
	}
	if err := b.working.BeginBlock(ref); err != nil {
		return err
	}
	b.working.Commit()
	b.base = b.working
	b.working = nil
	b.transactions = nil
	return nil
}

// ValidateCommit checks acceptance without publishing the working store.
func (b *TransactionBranch) ValidateCommit(ref domain.BlockRef, hashes []domain.Hash) error {
	if b.working == nil || ref.Hash != b.ref.Hash || ref.Number != b.ref.Number || ref.ParentHash != b.ref.ParentHash || !slices.Equal(hashes, b.transactions) {
		return fmt.Errorf("%w: pool block identity or transaction list does not match", ErrInvalidConfig)
	}
	return nil
}

// Base is the hash-verified pool store. Working is the tentative block view;
// it is nil outside an attempt. Read them only on the execution goroutine.
func (b *TransactionBranch) Base() *state.Store    { return b.base }
func (b *TransactionBranch) Working() *state.Store { return b.working }
