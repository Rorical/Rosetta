package ingest

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/solidlyv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

func applyLog(store *state.Store, log Log, key domain.PoolKey, block uint64) error {
	switch key.Kind {
	case domain.PoolKindCamelotV3:
		return applyCamelot(store, log, key, block)
	case domain.PoolKindSlipstream:
		current, ok := store.Pool(key)
		if ok {
			matched, err := slipstream.FollowMetadata(&current, log.Topics, log.Data)
			if err != nil {
				return err
			}
			if matched {
				return store.Upsert(current)
			}
		}
		return applyV3(store, log, key, block)
	case domain.PoolKindV3:
		// Slipstream shares the V3 Swap/Mint/Burn ABI.
		return applyV3(store, log, key, block)
	case domain.PoolKindSolidlyV3:
		return applySolidly(store, log, key, block)
	case domain.PoolKindV4:
		return applyV4(store, log, key, block)
	case domain.PoolKindV2:
		return applyV2(store, log, key, block)
	case domain.PoolKindVelodromeV2:
		return applyV2(store, log, key, block)
	case domain.PoolKindMetricOmm:
		return applyMetric(store, log, key, block)
	default:
		// Other families have no decoder in this unit. A resolved log is
		// skipped rather than misapplied.
		return nil
	}
}

func applyCamelot(store *state.Store, log Log, key domain.PoolKey, block uint64) error {
	event, matched, err := camelotv3.DecodeMetadata(log.Topics, log.Data)
	if err != nil {
		return fmt.Errorf("decode Camelot metadata: %w", err)
	}
	if !matched {
		return applyV3(store, log, key, block)
	}
	current, ok := store.Pool(key)
	if !ok {
		return fmt.Errorf("apply Camelot metadata: %w", state.ErrPoolNotFound)
	}
	if event.TickSpacing > 0 {
		current.TickSpacing = event.TickSpacing
	} else {
		current.Fee = event.Fee
		reverse := event.FeeOneForZero
		current.FeeOneForZero = &reverse
	}
	current.UpdatedBlock = block
	return store.Upsert(current)
}

// applySolidly follows SetFee, the only way a Solidly V3 fee changes, and
// reduces Swap/Mint/Burn through the shared V3 ABI.
func applySolidly(store *state.Store, log Log, key domain.PoolKey, block uint64) error {
	fee, matched, err := solidlyv3.DecodeSetFee(log.Topics, log.Data)
	if err != nil {
		return fmt.Errorf("decode Solidly V3 SetFee: %w", err)
	}
	if !matched {
		return applyV3(store, log, key, block)
	}
	current, ok := store.Pool(key)
	if !ok {
		return fmt.Errorf("apply Solidly V3 SetFee: %w", state.ErrPoolNotFound)
	}
	current.Fee = fee
	current.UpdatedBlock = block
	return store.Upsert(current)
}

func applyV3(store *state.Store, log Log, key domain.PoolKey, block uint64) error {
	ev, ok, err := uniswapv3.DecodeLog(log.Topics, log.Data)
	if err != nil {
		return fmt.Errorf("decode v3 log: %w", err)
	}
	if !ok || ev.Kind == uniswapv3.EventCollect {
		return nil
	}
	if err := store.ApplyV3(key, ev, block); err != nil {
		return fmt.Errorf("apply v3 event %d: %w", ev.Kind, err)
	}
	return nil
}

// applyV4 decodes a PoolManager event and reduces it through the V3 state
// shape. The decoded event carries no pool identity, so the key resolved from
// topic1 is the target; two pool IDs behind the manager stay separate. A zero
// delta ModifyLiquidity decodes as EventCollect and is skipped.
func applyV4(store *state.Store, log Log, key domain.PoolKey, block uint64) error {
	ev, ok, err := uniswapv4.DecodeEvent(log.Topics, log.Data)
	if err != nil {
		return fmt.Errorf("decode v4 log: %w", err)
	}
	if !ok || ev.Kind == uniswapv3.EventCollect {
		return nil
	}
	if err := store.ApplyV3(key, ev, block); err != nil {
		return fmt.Errorf("apply v4 event %d: %w", ev.Kind, err)
	}
	return nil
}

func applyV2(store *state.Store, log Log, key domain.PoolKey, block uint64) error {
	reserve0, reserve1, ok, err := uniswapv2.DecodeSync(log.Topics, log.Data)
	if key.Kind == domain.PoolKindVelodromeV2 {
		reserve0, reserve1, ok, err = velodromev2.DecodeSync(log.Topics, log.Data)
	}
	if err != nil {
		return fmt.Errorf("decode v2 sync: %w", err)
	}
	if !ok {
		return nil
	}

	current, found := store.Pool(key)
	if !found {
		return fmt.Errorf("apply v2 sync: %w: %s", state.ErrPoolNotFound, poolLabel(key))
	}
	current.Reserve0 = reserve0
	current.Reserve1 = reserve1
	current.UpdatedBlock = block
	if err := store.Upsert(current); err != nil {
		return fmt.Errorf("apply v2 sync: %w", err)
	}
	return nil
}

// poolLabel renders a key for an error message, including the V4 pool ID when
// the key has one.
func poolLabel(key domain.PoolKey) string {
	if key.PoolID == (domain.Hash{}) {
		return key.Address.Hex()
	}
	return key.Address.Hex() + "/" + key.PoolID.Hex()
}
