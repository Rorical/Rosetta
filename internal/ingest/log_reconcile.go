package ingest

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

// Reconcile verifies the observed prefix against a complete canonical block,
// then rebases and replays later blocks. A missing or changed receipt tail is
// repaired from canonical state if the streamed successor names the same seal.
// A different seal/parent discards the detached suffix. matched describes the
// receipt comparison, not finality of the still-tentative blocks.
func (b *LogBranch) Reconcile(ctx context.Context, base *state.Store, transactions []Transaction) (matched bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if base == nil {
		return false, fmt.Errorf("%w: nil log reconciliation base", ErrInvalidConfig)
	}
	previous, ref := b.base.Block(), base.Block()
	if previous.Number == ^uint64(0) || ref.Number != previous.Number+1 || ref.ParentHash != previous.Hash {
		return false, ErrFastContinuity
	}
	for i, tx := range transactions {
		if tx.Index != uint64(i) || tx.Hash == (domain.Hash{}) || !tx.Successful && len(tx.Logs) > 0 {
			return false, fmt.Errorf("%w: invalid canonical receipt %d", ErrInvalidConfig, i)
		}
	}
	if len(b.blocks) == 0 {
		return true, b.ResetTo(base)
	}
	observed := b.blocks[0]
	if observed.ref.Number != ref.Number || observed.ref.ParentHash != ref.ParentHash || observed.ref.Hash != (domain.Hash{}) && observed.ref.Hash != ref.Hash {
		return false, errors.Join(ErrFastConflict, b.ResetTo(base))
	}
	matched = len(observed.transactions) <= len(transactions)
	if observed.ref.Hash != (domain.Hash{}) && len(observed.transactions) != len(transactions) {
		matched = false
	}
	for i, tx := range observed.transactions {
		if i >= len(transactions) || transactionDigest(tx) != transactionDigest(transactions[i]) {
			matched = false
			break
		}
	}
	future := slices.Clone(b.blocks[1:])
	return matched, b.replaceBase(ctx, base, future)
}

// RefreshBase merges fee/tick/catalog updates at the same canonical anchor
// while preserving every observed block, including empty ones.
func (b *LogBranch) RefreshBase(ctx context.Context, base *state.Store) error {
	if base == nil || base.Block() != b.base.Block() {
		return ErrFastContinuity
	}
	return b.replaceBase(ctx, base, slices.Clone(b.blocks))
}

// Rewind replaces an in-flight payload without discarding earlier blocks.
// The caller must begin again at number and validate the new frame's parent.
func (b *LogBranch) Rewind(ctx context.Context, number uint64) error {
	index := len(b.blocks)
	for i, block := range b.blocks {
		if block.ref.Number == number {
			index = i
			break
		}
	}
	if index == len(b.blocks) {
		return ErrFastContinuity
	}
	return b.replaceBase(ctx, b.base, slices.Clone(b.blocks[:index]))
}

func (b *LogBranch) replaceBase(ctx context.Context, base *state.Store, blocks []logBlock) error {
	if err := b.ResetTo(base); err != nil {
		return err
	}
	for _, block := range blocks {
		// The next Begin supplies a predecessor's claimed seal. Never promote a
		// cached stream hash into the verified base or a StateDB identity.
		ref := block.ref
		ref.Hash = domain.Hash{}
		if err := b.Begin(ctx, ref); err != nil {
			return errors.Join(err, b.ResetTo(base))
		}
		for _, tx := range block.transactions {
			if _, err := b.Apply(ctx, tx); err != nil {
				return errors.Join(err, b.ResetTo(base))
			}
		}
		b.blocks[len(b.blocks)-1].ref.Hash = block.ref.Hash
	}
	return nil
}
