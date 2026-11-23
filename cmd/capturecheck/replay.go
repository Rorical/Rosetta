package main

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/state"
)

// replayStats describes what a pre-transaction replay applied.
type replayStats struct {
	Block         uint64
	PriorReceipts int
	AppliedLogs   int
}

// replayStep is one universe-pool event selected from a receipt before the
// target transaction. Receipt and Log locate the event in the block for errors.
type replayStep struct {
	Receipt int
	Log     uint32
	Pool    domain.PoolKey
	Event   uniswapv3.Event
}

// replayPriorEvents advances the store from the block-1 seed to the state the
// target transaction observed, by applying that block's universe-pool events.
// Alternatives are replayed too: the engine ranks them, so they need their
// pre-transaction state as well. The target transaction's own logs are
// excluded: they are the trade under test, not state it saw.
func replayPriorEvents(ctx context.Context, client *rpc.Client, cache *cachedCaller, store *state.Store, c caseData) (replayStats, error) {
	tx, err := domain.ParseHash(c.OnChain.Tx)
	if err != nil {
		return replayStats{}, fmt.Errorf("replay: onchain.tx %q: %w", c.OnChain.Tx, err)
	}

	header, err := cachedBlockByNumber(ctx, client, cache, c.Block)
	if err != nil {
		return replayStats{}, fmt.Errorf("replay: header %d: %w", c.Block, err)
	}
	receipts, err := cachedBlockReceipts(ctx, client, cache, header.Hash)
	if err != nil {
		return replayStats{}, fmt.Errorf("replay: receipts %d: %w", c.Block, err)
	}

	steps, target, err := selectPriorEvents(receipts, tx, c)
	if err != nil {
		return replayStats{}, fmt.Errorf("replay block %d: %w", c.Block, err)
	}
	for _, step := range steps {
		if err := store.ApplyV3(step.Pool, step.Event, c.Block); err != nil {
			return replayStats{}, fmt.Errorf("replay receipt %d log %d pool %s %s: %w",
				step.Receipt, step.Log, step.Pool.Address.Hex(), eventName(step.Event.Kind), err)
		}
	}
	return replayStats{Block: c.Block, PriorReceipts: target, AppliedLogs: len(steps)}, nil
}

// selectPriorEvents returns the universe-pool events of c that precede tx, in
// receipt then log order, plus the index of tx's receipt. Every log of every
// earlier receipt is considered: a swap or liquidity change on a seeded pool
// moves the state the target trade saw. Logs from other contracts, non-V3
// events, and collects are skipped because none of them change pool state.
func selectPriorEvents(receipts []rpc.Receipt, tx domain.Hash, c caseData) ([]replayStep, int, error) {
	pools, err := universePools(c.Universe)
	if err != nil {
		return nil, 0, err
	}
	target := -1
	for i, receipt := range receipts {
		if receipt.TransactionHash == tx {
			target = i
			break
		}
	}
	if target < 0 {
		return nil, 0, fmt.Errorf("transaction %s is not among the block's %d receipts", tx.Hex(), len(receipts))
	}

	var steps []replayStep
	for receiptIndex := 0; receiptIndex < target; receiptIndex++ {
		for _, log := range receipts[receiptIndex].Logs {
			key, ok := pools[log.Address]
			if !ok {
				continue
			}
			event, ok, err := uniswapv3.DecodeLog(log.Topics, log.Data)
			if err != nil {
				return nil, 0, fmt.Errorf("receipt %d log %d pool %s: %w",
					receiptIndex, log.LogIndex, key.Address.Hex(), err)
			}
			if !ok || event.Kind == uniswapv3.EventCollect {
				continue
			}
			steps = append(steps, replayStep{
				Receipt: receiptIndex,
				Log:     log.LogIndex,
				Pool:    key,
				Event:   event,
			})
		}
	}
	return steps, target, nil
}

// universePools maps each universe pool's address to its store key. Replay
// decodes Uniswap V3 logs, so a pool of another family is rejected rather than
// skipped: its state would silently stay at block-1.
func universePools(universe []poolSpec) (map[domain.Address]domain.PoolKey, error) {
	pools := make(map[domain.Address]domain.PoolKey, len(universe))
	for _, spec := range universe {
		switch spec.Kind {
		case domain.PoolKindV3, domain.PoolKindSlipstream:
		default:
			return nil, fmt.Errorf("replay: pool %s kind %s: only v3 and slipstream logs can be replayed",
				spec.Address, poolKindName(spec.Kind))
		}
		pools[spec.Address] = spec.key()
	}
	return pools, nil
}

// poolKindName names a pool family for replay errors.
func poolKindName(kind domain.PoolKind) string {
	switch kind {
	case domain.PoolKindV2:
		return "v2"
	case domain.PoolKindV3:
		return "v3"
	case domain.PoolKindV4:
		return "v4"
	case domain.PoolKindCamelotV3:
		return "camelot-v3"
	case domain.PoolKindSlipstream:
		return "slipstream"
	default:
		return fmt.Sprintf("kind %d", kind)
	}
}

// eventName names an event kind for replay errors.
func eventName(kind uniswapv3.EventKind) string {
	switch kind {
	case uniswapv3.EventInitialize:
		return "initialize"
	case uniswapv3.EventMint:
		return "mint"
	case uniswapv3.EventBurn:
		return "burn"
	case uniswapv3.EventSwap:
		return "swap"
	case uniswapv3.EventCollect:
		return "collect"
	default:
		return fmt.Sprintf("kind %d", kind)
	}
}
