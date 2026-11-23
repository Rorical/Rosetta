package exec

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
)

// FastReplayEvidence records the exact temporary execution and trusted input
// source used for confirmation. Hashes identify content; the consumer must
// independently select its trusted source and observe current stream counters.
type FastReplayEvidence struct {
	Version         domain.StateVersion
	ContextSource   domain.Hash
	ContextHash     domain.Hash
	StateRoot       domain.Hash
	HeaderHash      domain.Hash
	Transactions    uint64
	Generation      uint64
	ReceiptRevision uint64
	BlockGasLeft    uint64
}

type FastDispatchContext struct {
	Version         domain.StateVersion
	ContextSource   domain.Hash
	Generation      uint64
	ReceiptRevision uint64
}

// ValidateFastForDispatch requires live stream observations in addition to the
// ordinary version/policy/time gate. File output alone cannot supply freshness.
func ValidateFastForDispatch(ctx context.Context, plan *ReadyPlan, current FastDispatchContext, chainID uint64, graph uint64, policy domain.Hash, nowUnixMS int64, models ...FeeModel) error {
	if plan == nil || plan.FastReplay == nil || current.Version.Source != domain.SourceFast ||
		plan.FastReplay.ContextSource != current.ContextSource || plan.FastReplay.Generation != current.Generation ||
		plan.FastReplay.ReceiptRevision != current.ReceiptRevision {
		return fmt.Errorf("Fast plan does not match the trusted context source or current stream")
	}
	return validateForDispatch(ctx, plan, current.Version, chainID, graph, policy, nowUnixMS, models...)
}

func validatePlanSource(plan *ReadyPlan) error {
	if plan.Version.Source == domain.SourceStandard {
		if plan.FastReplay != nil || plan.Flashblocks != nil || plan.Evidence.Context.Block.Hash != plan.Version.Anchor.Hash ||
			plan.Evidence.Context.Block.Number != plan.Version.BlockNumber {
			return fmt.Errorf("standard execution evidence differs from the verified block")
		}
		return nil
	}
	if plan.Flashblocks != nil {
		return validateFlashblocksPlan(plan)
	}
	f := plan.FastReplay
	if plan.Version.Source != domain.SourceFast || f == nil || f.Version != plan.Version ||
		f.ContextSource == (domain.Hash{}) || f.ContextHash == (domain.Hash{}) || f.StateRoot == (domain.Hash{}) ||
		f.HeaderHash == (domain.Hash{}) || f.HeaderHash != plan.Evidence.Context.Block.Hash ||
		plan.Evidence.Context.Block.Number != plan.Version.BlockNumber || plan.Version.BlockNumber <= plan.Version.Anchor.Number ||
		plan.Version.Epoch == 0 || f.ReceiptRevision == 0 || f.Transactions == 0 || f.Transactions > 4096 ||
		plan.Version.TxIndex >= f.Transactions || plan.Evidence.Call.GasLimit > f.BlockGasLeft {
		return fmt.Errorf("Fast execution evidence does not bind a complete temporary prefix")
	}
	return nil
}
