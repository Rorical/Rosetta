package bootstrap

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

// SetHead opens and commits a head frame with no pools. It is the bootstrap
// path for a discovery-only configuration whose lookback found nothing yet: the
// store must record the resolved head so the next feed block extends it, even
// though no pool exists to seed.
//
// The store must be empty: a zero head and no pools. An identical head already
// set is accepted as a no-op; any other non-zero head or an existing pool is
// rejected so a caller cannot silently move a store that already holds state.
func SetHead(ctx context.Context, store *state.Store, ref domain.BlockRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if store == nil {
		return fmt.Errorf("%w: nil store", ErrInvalidSpec)
	}
	if err := validateRef(ref); err != nil {
		return err
	}
	head := store.Block()
	if head.Number != 0 || head.Hash != (domain.Hash{}) {
		if head == ref && len(store.Pools()) == 0 {
			return nil
		}
		return fmt.Errorf("%w: store head is already block %d hash %s, want %d hash %s",
			ErrInvalidSpec, head.Number, head.Hash.Hex(), ref.Number, ref.Hash.Hex())
	}
	if pools := len(store.Pools()); pools > 0 {
		return fmt.Errorf("%w: store already holds %d pools", ErrInvalidSpec, pools)
	}
	if err := store.BeginBlock(ref); err != nil {
		return fmt.Errorf("bootstrap: record head block %d: %w", ref.Number, err)
	}
	store.Commit()
	return nil
}
