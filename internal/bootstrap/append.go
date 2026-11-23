package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// AppendBatch loads the specs the store does not already hold, at ref, which
// must equal the current store head. It skips keys already present, leaves the
// head number alone, and writes nothing when every spec is already stored.
//
// Every missing spec shares one undo frame opened at ref and one commit, so a
// scan that adds many pools consumes one slot of the bounded reorg history
// instead of one per pool. The returned slice names the specs the frame seeded;
// it is empty when there was nothing to do.
//
// Any failure rolls the whole batch back, so a partially appended set is never
// observed and the existing undo history is kept. Restore is only the fallback
// when the rollback itself fails, because Restore clears the history.
func AppendBatch(ctx context.Context, caller pool.ContractCaller, store *state.Store, specs []Spec, ref domain.BlockRef, wordRadius int16) ([]Spec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if caller == nil {
		return nil, fmt.Errorf("%w: nil contract caller", ErrInvalidSpec)
	}
	if store == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalidSpec)
	}
	if err := validateRef(ref); err != nil {
		return nil, err
	}
	if len(specs) > 0 {
		manager, _ := uniswapv4.Deployment(caller)
		if err := validateSpecs(specs, manager); err != nil {
			return nil, err
		}
	}
	head := store.Block()
	if head.Number != ref.Number || head.Hash != ref.Hash {
		return nil, fmt.Errorf("%w: append block %d hash %s does not match store head %d hash %s",
			ErrInvalidSpec, ref.Number, ref.Hash.Hex(), head.Number, head.Hash.Hex())
	}

	missing := make([]Spec, 0, len(specs))
	for _, spec := range specs {
		if _, ok := store.Pool(spec.key()); ok {
			continue
		}
		missing = append(missing, spec)
	}
	if len(missing) == 0 {
		return nil, nil
	}

	checkpoint := store.Checkpoint()
	if err := store.BeginBlock(ref); err != nil {
		return nil, fmt.Errorf("bootstrap: begin append block %d: %w", ref.Number, err)
	}
	if err := seedAll(ctx, caller, store, missing, ref, wordRadius); err != nil {
		return nil, failAppend(store, checkpoint, err)
	}
	store.Commit()
	return missing, nil
}

// Append adds every spec through AppendBatch. It is the one-pool entry point;
// callers that may add several pools use AppendBatch so they consume one undo
// frame.
func Append(ctx context.Context, caller pool.ContractCaller, store *state.Store, specs []Spec, ref domain.BlockRef, wordRadius int16) error {
	_, err := AppendBatch(ctx, caller, store, specs, ref, wordRadius)
	return err
}

// failAppend undoes the append frame and joins every failure so the original
// cause is never lost. Rollback keeps the undo history, so it is the only
// cleanup on the normal path; Restore is the fallback when the rollback fails,
// accepting the history loss to avoid leaving a partial batch in place.
func failAppend(store *state.Store, checkpoint state.Snapshot, cause error) error {
	rollbackErr := store.Rollback()
	if rollbackErr == nil {
		return cause
	}
	errs := []error{cause, fmt.Errorf("bootstrap: rollback append: %w", rollbackErr)}
	if restoreErr := store.Restore(checkpoint); restoreErr != nil {
		errs = append(errs, fmt.Errorf("bootstrap: restore snapshot: %w", restoreErr))
	}
	return errors.Join(errs...)
}

// seedAll loads every spec at ref.Number and stamps deep state with ref. The
// caller owns the checkpoint and the undo frame.
//
// Pools load concurrently because a remote RPC makes each pool's reads a
// serial chain of round trips: 1,000 pools at 150 ms per round trip take most
// of an hour one at a time. Each
// load writes into its own buffer and the buffers reach the store in spec
// order, so the result does not depend on scheduling. A failure is reported
// for the first failing spec in that order after every load has finished;
// cancelling the others early would let their context errors hide it.
func seedAll(ctx context.Context, caller pool.ContractCaller, store *state.Store, specs []Spec, ref domain.BlockRef, wordRadius int16) error {
	block := fmt.Sprintf("0x%x", ref.Number)
	buffers := make([]pendingSeed, len(specs))
	failures := make([]error, len(specs))
	slots := make(chan struct{}, seedParallelism)
	var wg sync.WaitGroup
	for i, spec := range specs {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			failures[i] = seedPool(ctx, caller, &buffers[i], spec, block, ref, wordRadius)
		}()
	}
	wg.Wait()
	for i, spec := range specs {
		if failures[i] != nil {
			return failures[i]
		}
		if err := buffers[i].apply(store); err != nil {
			return fmt.Errorf("bootstrap: store pool %s: %w", spec.label(), err)
		}
	}
	return nil
}

// seedParallelism bounds concurrent pool loads against one RPC provider.
const seedParallelism = 8

// poolSink receives one seeded pool's state and deep state.
type poolSink interface {
	Upsert(pool.State) error
	SetDeep(domain.PoolKey, pool.DeepState) error
}

// pendingSeed records one pool's writes until seedAll applies them in order.
type pendingSeed struct {
	writes []func(*state.Store) error
}

func (p *pendingSeed) Upsert(loaded pool.State) error {
	p.writes = append(p.writes, func(store *state.Store) error { return store.Upsert(loaded) })
	return nil
}

func (p *pendingSeed) SetDeep(key domain.PoolKey, deep pool.DeepState) error {
	p.writes = append(p.writes, func(store *state.Store) error { return store.SetDeep(key, deep) })
	return nil
}

func (p *pendingSeed) apply(store *state.Store) error {
	for _, write := range p.writes {
		if err := write(store); err != nil {
			return err
		}
	}
	return nil
}
