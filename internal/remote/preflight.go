package remote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/rpc"
)

var errStreamChecked = errors.New("Flashblocks stream checked")

// Check the logs capability and chain parent before expensive pool bootstrap.
// A stripped or wrong-chain stream must not trigger thousands of paid tick reads.
func checkFlashblocksStream(ctx context.Context, endpoint string, client *rpc.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := (feed.FlashClient{URL: endpoint, AllowMissingReceipts: true}).Run(ctx, func() error { return nil }, func(block feed.Flashblock) error {
		if block.MissingReceipts {
			return fmt.Errorf("Flashblocks stream omits receipts; select a complete-logs stream or explicitly enable -flashblocks-receipts-rpc")
		}
		if block.Base == nil {
			return nil
		}
		// The stream can name a parent the RPC serves a moment later.
		parent, err := waitExecutionBlock(ctx, client, block.Base.Parent, 2*time.Second, 100*time.Millisecond)
		if err != nil {
			return fmt.Errorf("Flashblocks parent is unavailable on selected RPC: %w", err)
		}
		if parent.Ref.Number+1 != block.Number {
			return fmt.Errorf("Flashblocks parent height differs from selected RPC")
		}
		return errStreamChecked
	})
	if errors.Is(err, errStreamChecked) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("Flashblocks capability check: %w", ctx.Err())
}
