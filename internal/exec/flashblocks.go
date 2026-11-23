package exec

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
)

// FlashblocksEvidence identifies an exact transaction prefix. StateSource is
// empty for native replay and rpc-cache-v1 for an atomic node cache export.
// OverlayHash is a state override commitment, not a trie state root.
type FlashblocksEvidence struct {
	StateSource  string `json:",omitempty"`
	Version      domain.StateVersion
	PayloadID    string
	Index        uint64
	Transactions uint64
	Source       domain.Hash
	ContextHash  domain.Hash
	RawBlockHash domain.Hash
	OverlayHash  domain.Hash
	BlockGasLeft uint64
}

// FlashblocksDispatchContext must be observed from the currently connected
// stream. A serialized plan cannot establish that its prefix remains current.
type FlashblocksDispatchContext struct {
	Version   domain.StateVersion
	PayloadID string
	Index     uint64
	Source    domain.Hash
}

func ValidateFlashblocksForDispatch(ctx context.Context, plan *ReadyPlan, current FlashblocksDispatchContext, chainID uint64, graph uint64, policy domain.Hash, nowUnixMS int64, models ...FeeModel) error {
	if plan == nil || plan.Flashblocks == nil || current.Version.Source != domain.SourceFast || plan.Flashblocks.PayloadID != current.PayloadID || plan.Flashblocks.Index != current.Index || plan.Flashblocks.Source != current.Source {
		return fmt.Errorf("Flashblocks plan differs from current stream position")
	}
	return validateForDispatch(ctx, plan, current.Version, chainID, graph, policy, nowUnixMS, models...)
}

func validateFlashblocksPlan(plan *ReadyPlan) error {
	f := plan.Flashblocks
	if f != nil {
		if f.StateSource != "" && f.StateSource != "rpc-cache-v1" {
			return fmt.Errorf("unsupported Flashblocks state source")
		}
		payload, err := hex.DecodeString(strings.TrimPrefix(f.PayloadID, "0x"))
		if err != nil || !strings.HasPrefix(f.PayloadID, "0x") || len(payload) != 8 {
			return fmt.Errorf("invalid Flashblocks payload identity")
		}
	}
	if f == nil || plan.FastReplay != nil || plan.Version.Source != domain.SourceFast || plan.Evidence.Context.Family != domain.FamilyOPStack || f.Version != plan.Version ||
		len(f.PayloadID) != 18 || f.Source == (domain.Hash{}) || f.ContextHash == (domain.Hash{}) || f.RawBlockHash == (domain.Hash{}) || f.OverlayHash == (domain.Hash{}) ||
		plan.Version.Anchor.Hash == (domain.Hash{}) || plan.Version.BlockNumber != plan.Version.Anchor.Number+1 || plan.Version.Epoch == 0 ||
		f.Transactions == 0 || f.Transactions > 4096 || plan.Version.TxIndex != f.Transactions-1 || plan.Evidence.Context.Block.Number != plan.Version.BlockNumber ||
		plan.Evidence.Context.Block.ParentHash != plan.Version.Anchor.Hash || plan.Evidence.Context.Block.Hash != f.ContextHash || plan.Evidence.Call.GasLimit > f.BlockGasLeft || f.BlockGasLeft > plan.Evidence.Context.BlockGasLimit {
		return fmt.Errorf("Flashblocks execution evidence does not bind a complete RPC state")
	}
	return nil
}
