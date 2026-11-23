// Command fastprobe measures an Arbitrum Fast Feed connection before Rosetta
// uses it: it records frames for a number of blocks, reads the same blocks'
// canonical receipts, and reports the index mapping for -fast-first-index and
// -fast-canonical-first-index, its consistency, frames and PGA rounds per
// block, and arrival times. It never signs or sends.
//
// Usage:
//
//	ROSETTA_FAST_FEED_KEY=... ROSETTA_RPC_URL=... fastprobe -blocks 200 -out frames.jsonl
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fastprobe:", err)
		os.Exit(1)
	}
}

func run() error {
	feedURL := flag.String("feed", "wss://arb1-txfeed.arbitrum.io/", "Fast Feed WebSocket URL")
	blocks := flag.Int("blocks", 200, "complete blocks to observe")
	timeout := flag.Duration("timeout", 10*time.Minute, "give up after this long")
	outPath := flag.String("out", "", "optional new JSONL file for every received frame")
	flag.Parse()
	key, rpcURL := os.Getenv("ROSETTA_FAST_FEED_KEY"), os.Getenv("ROSETTA_RPC_URL")
	if key == "" || rpcURL == "" || *blocks < 1 {
		return errors.New("set ROSETTA_FAST_FEED_KEY and ROSETTA_RPC_URL; -blocks must be positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	frames, err := collect(ctx, *feedURL, key, *blocks+2)
	if err != nil {
		return err
	}
	if *outPath != "" {
		if err := writeFrames(*outPath, frames); err != nil {
			return err
		}
	}
	// Give the canonical RPC time to serve the last observed block.
	time.Sleep(3 * time.Second)
	canonical, err := receipts(context.Background(), rpc.NewClient(rpcURL, rpc.Options{RetryReads: true, Timeout: 20 * time.Second, MaxRetries: 5, Backoff: time.Second}), frames)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(analyze(frames, canonical))
}

// collect reads frames until it has seen wantBlocks distinct block numbers,
// then stops at the first frame of the next block so the last block is whole.
func collect(ctx context.Context, url, key string, wantBlocks int) ([]observation, error) {
	client, err := feed.NewFastClient(url, key, feed.ClientOptions{ChainID: domain.ArbitrumOneChainID})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var frames []observation
	seen := make(map[uint64]bool)
	done := false
	handler := func(message feed.FastMessage) error {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return nil
		}
		if !seen[message.BlockNumber] && len(seen) == wantBlocks {
			done = true
			cancel()
			return nil
		}
		seen[message.BlockNumber] = true
		frames = append(frames, observation{
			ReceivedMS: time.Now().UnixMilli(), TimestampMS: message.TimestampMS, PGARound: message.PGARound,
			BlockNumber: message.BlockNumber, TxIndex: message.TxIndex, TxHash: message.TxHash, Logs: len(message.Receipt.Logs),
		})
		return nil
	}
	// A reconnect loses frames; one run measures a single connection.
	invalidations := 0
	invalidate := func() error {
		invalidations++
		if invalidations > 1 && !done {
			return errors.New("fast feed connection lost; rerun the probe")
		}
		return nil
	}
	runErr := client.Run(ctx, handler, invalidate)
	mu.Lock()
	defer mu.Unlock()
	if !done {
		return nil, errors.Join(errors.New("stopped before observing the requested blocks"), runErr)
	}
	return frames, nil
}

func receipts(ctx context.Context, client *rpc.Client, frames []observation) (map[uint64]canonicalBlock, error) {
	numbers := make(map[uint64]bool)
	for _, frame := range frames {
		numbers[frame.BlockNumber] = true
	}
	out := make(map[uint64]canonicalBlock, len(numbers))
	for number := range numbers {
		var list []struct {
			Hash  string `json:"transactionHash"`
			Index string `json:"transactionIndex"`
		}
		if err := client.Call(ctx, "eth_getBlockReceipts", []any{"0x" + strconv.FormatUint(number, 16)}, &list); err != nil {
			return nil, fmt.Errorf("block %d receipts: %w", number, err)
		}
		block := canonicalBlock{Number: number, Hashes: make([]domain.Hash, len(list))}
		for i, receipt := range list {
			if index, err := strconv.ParseUint(receipt.Index, 0, 64); err != nil || index != uint64(i) {
				return nil, fmt.Errorf("block %d receipt %d has index %q", number, i, receipt.Index)
			}
			hash, err := domain.ParseHash(receipt.Hash)
			if err != nil {
				return nil, fmt.Errorf("block %d receipt %d hash: %w", number, i, err)
			}
			block.Hashes[i] = hash
		}
		out[number] = block
	}
	return out, nil
}

func writeFrames(path string, frames []observation) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, frame := range frames {
		if err := encoder.Encode(frame); err != nil {
			return errors.Join(err, file.Close())
		}
	}
	return file.Close()
}
