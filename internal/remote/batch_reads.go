package remote

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func (s *snapshot) CallContracts(ctx context.Context, calls []pool.ContractCall) ([]string, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	keys := make([]string, len(calls))
	queries := make(map[string]rpc.Call, len(calls))
	for i, call := range calls {
		if call.Block != "0x"+strconv.FormatUint(s.block.Ref.Number, 16) {
			return nil, fmt.Errorf("remote loader requested another snapshot")
		}
		params := s.contractParams(call.To, call.Data, nil)
		raw, err := json.Marshal([]any{"eth_call", params})
		if err != nil {
			return nil, err
		}
		keys[i] = string(raw)
		queries[keys[i]] = rpc.Call{Method: "eth_call", Params: params}
	}
	return s.readCache().LoadBatch(ctx, keys, func(ctx context.Context, missing []string) ([]rpc.ReadResult, error) {
		values := make([]rpc.ReadResult, len(missing))
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			stop error // the first failure; chunks after it are not sent
		)
		slots := make(chan struct{}, batchInFlight)
		for start := 0; start < len(missing); start += batchChunk {
			end := min(start+batchChunk, len(missing))
			slots <- struct{}{}
			mu.Lock()
			halted := stop
			mu.Unlock()
			if halted != nil {
				<-slots
				// Completed chunks keep their values; retrying the
				// acquisition must not pay for them again.
				for i := start; i < end; i++ {
					values[i].Err = halted
				}
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				if err := s.fetchChunk(ctx, missing[start:end], queries, values[start:end]); err != nil {
					mu.Lock()
					if stop == nil {
						stop = err
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		return values, nil
	})
}

const (
	// batchChunk is the most calls one JSON-RPC batch carries.
	batchChunk = 8
	// batchInFlight bounds the concurrent chunks of one load. A pool with a
	// wide tick window needs hundreds of chunks, and one at a time each waits
	// a full round trip (typically about 150 ms to a hosted RPC provider for
	// state at a past block), so one fine-spaced pool could hold a whole
	// bootstrap for minutes.
	batchInFlight = 4
)

// fetchChunk sends one batch and fills values. It returns the transport error,
// or the first member error, after recording every member's own result.
func (s *snapshot) fetchChunk(ctx context.Context, keys []string, queries map[string]rpc.Call, values []rpc.ReadResult) error {
	batch := make([]rpc.Call, len(keys))
	for i, key := range keys {
		batch[i] = queries[key]
	}
	results, err := s.client.Batch(ctx, batch)
	if err != nil {
		for i := range values {
			values[i].Err = err
		}
		return err
	}
	var memberError error
	for i, result := range results {
		if result.Error != nil {
			values[i].Err = result.Error
			memberError = cmp.Or(memberError, error(result.Error))
			continue
		}
		values[i].Err = json.Unmarshal(result.Result, &values[i].Value)
	}
	return memberError
}
