package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// followFlashblocks completes stripped receipts outside the state writer.
// The synchronous transport callback bounds queued raw frames and RPC work.
// Failed completion abandons the prefix. With seals non-nil it also assembles
// whole blocks for the canonical follower.
func followFlashblocks(ctx context.Context, url string, client *rpc.Client, allowHydration bool, out chan<- streamEvent, inputRevision *atomic.Uint64, seals chan<- flashSeal) error {
	var cursor flashCursor
	assembler := flashAssembler{out: seals}
	reset := func() error {
		cursor = flashCursor{}
		assembler.reset()
		inputRevision.Add(1)
		return sendReset(ctx, out)
	}
	send := func(event streamEvent) error {
		select {
		case out <- event:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	observer := telemetry.FromContext(ctx).ConnectionObserver(telemetry.Fast)
	return (feed.FlashClient{URL: url, AllowMissingReceipts: true, OnConnection: observer}).Run(ctx, reset, func(block feed.Flashblock) error {
		receivedAt := time.Now()
		encoded, err := json.Marshal(block)
		if err != nil {
			return err
		}
		hash := digest(encoded)
		if cursor.active && cursor.payload == block.PayloadID && cursor.index == block.Index {
			if cursor.digest == hash {
				return nil
			}
			if err := reset(); err != nil {
				return err
			}
			if block.Index != 0 {
				return nil
			}
		}
		if block.Index == 0 {
			cursor = flashCursor{payload: block.PayloadID, number: block.Number, active: true}
		} else if !cursor.active || cursor.payload != block.PayloadID || cursor.number != block.Number || block.Index != cursor.index+1 {
			return reset()
		}
		revision := inputRevision.Add(1)
		hydrated := block.MissingReceipts
		if block.MissingReceipts {
			if !allowHydration {
				return fmt.Errorf("Flashblocks stream omits receipts; select a complete-logs stream or explicitly enable -flashblocks-receipts-rpc")
			}
			// Receipt I/O may span a new frame interval. Retire the preceding plan as
			// soon as this raw frame is observed, before waiting for any HTTP response.
			if err := send(streamEvent{flashPending: true}); err != nil {
				return err
			}
			receiptCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			completed, err := completeFlashReceipts(receiptCtx, client, block, cursor.nextTransaction)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.Warn("Flashblocks receipt completion failed", "block", block.Number, "index", block.Index, "error", err)
				return reset()
			}
			block = completed
		}
		if seals != nil {
			assembler.frame(block, hydrated)
		}
		if err := send(streamEvent{flash: &block, receivedAt: receivedAt, revision: revision}); err != nil {
			return err
		}
		cursor.index, cursor.digest = block.Index, hash
		cursor.nextTransaction += uint64(len(block.Transactions))
		return nil
	})
}

// retireFlashResults leaves the previous pool prefix intact so the newly
// observed raw frame can extend it after complete receipts arrive.
func (r *runtime) retireFlashResults() {
	r.tentative = domain.StateVersion{}
	if r.flashConfirmation != nil {
		r.flashConfirmation.Invalidate()
	}
	if r.worker != nil {
		r.worker.Invalidate(domain.SourceFast)
		r.search.TentativePending = domain.StateVersion{}
	}
}

func completeFlashReceipts(ctx context.Context, client *rpc.Client, block feed.Flashblock, first uint64) (feed.Flashblock, error) {
	if !block.MissingReceipts {
		return block, nil
	}
	block.Transactions = slices.Clone(block.Transactions)
	pending := make([]int, len(block.Transactions))
	for i := range pending {
		pending[i] = i
	}
	for round := 0; len(pending) > 0; round++ {
		var next []int
		for start := 0; start < len(pending); start += 128 {
			indices := pending[start:min(start+128, len(pending))]
			calls := make([]rpc.Call, len(indices))
			for i, index := range indices {
				calls[i] = rpc.Call{Method: "eth_getTransactionReceipt", Params: []any{block.Transactions[index].Hash.Hex()}}
			}
			results, err := client.Batch(ctx, calls)
			if err != nil {
				return feed.Flashblock{}, err
			}
			for i, result := range results {
				if result.Error != nil {
					return feed.Flashblock{}, result.Error
				}
				index := indices[i]
				if string(result.Result) == "null" {
					next = append(next, index)
					continue
				}
				success, logs, err := decodeFlashRPCReceipt(result.Result, block.Number, first+uint64(index), block.Transactions[index].Hash)
				if err != nil {
					return feed.Flashblock{}, err
				}
				block.Transactions[index].Successful, block.Transactions[index].Logs = success, logs
			}
		}
		pending = next
		if len(pending) > 0 {
			if round == 2 {
				return feed.Flashblock{}, fmt.Errorf("Flashblocks receipts still unavailable after three rounds")
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return feed.Flashblock{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	block.MissingReceipts = false
	return block, nil
}

func decodeFlashRPCReceipt(raw json.RawMessage, number, index uint64, hash domain.Hash) (bool, []feed.FastLog, error) {
	var receipt struct {
		Hash   string `json:"transactionHash"`
		Number string `json:"blockNumber"`
		Index  string `json:"transactionIndex"`
		Status string `json:"status"`
		Logs   *[]struct {
			Address string    `json:"address"`
			Topics  *[]string `json:"topics"`
			Data    *string   `json:"data"`
			Hash    string    `json:"transactionHash"`
			Number  string    `json:"blockNumber"`
			Index   string    `json:"transactionIndex"`
			Removed bool      `json:"removed"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return false, nil, err
	}
	height, heightErr := quantity(receipt.Number)
	position, positionErr := quantity(receipt.Index)
	status, statusErr := quantity(receipt.Status)
	if receipt.Hash != hash.Hex() || heightErr != nil || height != number || positionErr != nil || position != index || statusErr != nil || status > 1 || receipt.Logs == nil || status == 0 && len(*receipt.Logs) != 0 {
		return false, nil, fmt.Errorf("Flashblocks RPC receipt identity/status mismatch")
	}
	logs := make([]feed.FastLog, 0, len(*receipt.Logs))
	for _, entry := range *receipt.Logs {
		address, err := domain.ParseAddress(entry.Address)
		if err != nil {
			return false, nil, err
		}
		if entry.Removed || entry.Hash != hash.Hex() || entry.Number != receipt.Number || entry.Index != receipt.Index || entry.Topics == nil || len(*entry.Topics) > 4 || entry.Data == nil {
			return false, nil, fmt.Errorf("Flashblocks RPC log identity mismatch")
		}
		data, err := hexData(*entry.Data)
		if err != nil {
			return false, nil, err
		}
		log := feed.FastLog{Address: address, Data: data}
		for _, text := range *entry.Topics {
			topic, err := domain.ParseHash(text)
			if err != nil {
				return false, nil, err
			}
			log.Topics = append(log.Topics, topic)
		}
		logs = append(logs, log)
	}
	return status == 1, logs, nil
}
