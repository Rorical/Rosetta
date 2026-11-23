package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/Rorical/Rosetta/internal/state"
)

// RefreshBase installs enriched dependencies for the same accepted block and
// replays the owned tentative receipt cache. Existing prefix tickets retire;
// stream positions remain contiguous, so no next-block restart is required.
func (b *FastBranch) RefreshBase(ctx context.Context, base *state.Store) error {
	if base == nil || base.Block() != b.base.Block() {
		return fmt.Errorf("%w: dependency refresh changed canonical anchor", ErrFastContinuity)
	}
	records := b.records
	if err := b.ResetTo(base); err != nil {
		return err
	}
	for _, record := range records {
		if _, err := b.Apply(ctx, record.message); err != nil {
			return errors.Join(err, b.ResetTo(base))
		}
	}
	return nil
}
