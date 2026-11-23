package remote

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Paper pins fee reads to the selected world state before adding virtual
// executor/operator state. Their setup must not invalidate fee oracle reads.
// The ordinary live confirmation path continues to use its current snapshot.
func (s *snapshot) startPaperFeePrefetch(ctx context.Context, gas uint64) <-chan paperFeePrefetchResult {
	state := s.Retain().(*snapshot)
	s.paperFeeState = state
	done := make(chan paperFeePrefetchResult, 1)
	go func() {
		started := time.Now()
		tasks := []func() error{
			func() error {
				code, err := state.CodeHash(ctx, opGasOracle)
				if err == nil && code == ([32]byte{}) {
					err = fmt.Errorf("OP fee oracle code unavailable")
				}
				return err
			},
			func() error { _, err := state.readWord(ctx, opL1Block, "number()"); return err },
			func() error { _, err := state.readWord(ctx, opGasOracle, "l1BaseFee()"); return err },
			func() error { _, err := state.opFeeWord(ctx, "getOperatorFee(uint256)", gas); return err },
		}
		failures := make([]error, len(tasks))
		var group sync.WaitGroup
		for i, task := range tasks {
			group.Add(1)
			go func() { defer group.Done(); failures[i] = task() }()
		}
		group.Wait()
		done <- paperFeePrefetchResult{duration: time.Since(started), err: errors.Join(failures...)}
	}()
	return done
}

type paperFeePrefetchResult struct {
	duration time.Duration
	err      error
}
