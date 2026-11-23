package remote

import (
	"context"

	"github.com/Rorical/Rosetta/internal/domain"
)

func (r *runtime) acceptStreamEvent(ctx context.Context, event streamEvent, wake chan<- domain.Hash) error {
	r.processingRevision = event.revision
	if event.flashPending {
		r.retireFlashResults()
		return nil
	}
	if event.reset {
		err := r.resetTentative()
		close(event.accepted)
		if err != nil {
			return err
		}
		return nil
	}
	if event.fast != nil {
		return r.acceptFastMessage(ctx, *event.fast, event.receivedAt, event.revision)
	}
	if event.flash.Index == 0 && event.flash.Base != nil && event.flash.Base.Parent != r.store.Block().Hash {
		select {
		case wake <- event.flash.Base.Parent:
		default:
		}
	}
	return r.acceptFlashblock(ctx, *event.flash, event.receivedAt)
}
