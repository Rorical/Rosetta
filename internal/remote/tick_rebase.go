package remote

import (
	"context"

	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func rebaseRepair(ctx context.Context, current state.View, answer tickAnswer, tail []canonicalUpdate) (pool.DeepState, bool, error) {
	p := answer.task.state
	latest, ok := current.Pool(p.Key)
	if !ok || p.Token0 != latest.Token0 || p.Token1 != latest.Token1 || p.TickSpacing != latest.TickSpacing {
		return pool.DeepState{}, false, nil
	}
	store := state.New()
	if err := store.BeginBlock(answer.task.reader.block.Ref); err != nil {
		return pool.DeepState{}, false, err
	}
	if err := store.Upsert(p); err != nil {
		return pool.DeepState{}, false, err
	}
	if err := store.SetDeep(p.Key, answer.deep); err != nil {
		return pool.DeepState{}, false, err
	}
	store.Commit()
	store.ClearDirty()
	branch, err := ingest.NewTransactionBranch(store)
	if err != nil {
		return pool.DeepState{}, false, err
	}
	for _, update := range tail {
		if update.block.Ref.Number <= branch.Base().Block().Number {
			continue
		}
		if update.block.Ref.ParentHash != branch.Base().Block().Hash {
			return pool.DeepState{}, false, nil
		}
		if err = branch.Begin(update.block.Ref); err != nil {
			return pool.DeepState{}, false, err
		}
		for _, tx := range update.transactions {
			if _, err = branch.Apply(ctx, tx); err != nil {
				return pool.DeepState{}, false, err
			}
		}
		if err = branch.Commit(update.block.Ref, update.block.Transactions); err != nil {
			return pool.DeepState{}, false, err
		}
	}
	if branch.Base().Block() != current.Block() {
		return pool.DeepState{}, false, nil
	}
	got, _ := branch.Base().Pool(p.Key)
	if got.Tick != latest.Tick || got.SqrtPriceX96.Cmp(latest.SqrtPriceX96) != 0 || got.Liquidity.Cmp(latest.Liquidity) != 0 {
		return pool.DeepState{}, false, nil
	}
	deep, ok := branch.Base().View().Deep(p.Key)
	if !ok {
		return pool.DeepState{}, false, nil
	}
	deep.LoadedBlock = current.Block()
	return deep, true, nil
}
