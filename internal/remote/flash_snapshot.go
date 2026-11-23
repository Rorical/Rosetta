package remote

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// bindFlashSnapshot fixes all later reads to a canonical parent and one owned
// overlay. Both native replay and atomic node cache export use this boundary.
func bindFlashSnapshot(client *rpc.Client, anchor rpc.ExecutionBlock, chain domain.ChainIdentity, deployments domain.Deployments, prefix flashPrefix, raw []byte, header *flashHeader, overlay map[string]any) (*snapshot, *execution.FlashblocksEvidence, error) {
	contextHash := prefix.contextHash()
	base := prefix.Base.Execution
	s := &snapshot{client: client, chain: chain, deployments: deployments}
	s.anchor = &anchor
	s.block = rpc.ExecutionBlock{
		Ref:     domain.BlockRef{Number: prefix.Base.Number, Hash: contextHash, ParentHash: anchor.Ref.Hash, Timestamp: prefix.Base.Timestamp},
		BaseFee: new(big.Int).Set(base.BaseFee), GasLimit: base.GasLimit, Coinbase: base.Coinbase,
	}
	s.overlay = overlay
	s.blockOverrides = map[string]any{
		"number": fmt.Sprintf("0x%x", header.Number), "time": fmt.Sprintf("0x%x", header.Time),
		"gasLimit": fmt.Sprintf("0x%x", header.GasLimit), "feeRecipient": base.Coinbase.Hex(),
		"prevRandao": base.Random.Hex(), "baseFeePerGas": "0x" + base.BaseFee.Text(16),
	}
	hash, err := overlayHash(overlay)
	if err != nil {
		return nil, nil, err
	}
	proof := &execution.FlashblocksEvidence{
		Version: prefix.Version, PayloadID: prefix.Payload, Index: prefix.Index,
		Transactions: uint64(len(prefix.Transactions)), Source: prefix.Source,
		ContextHash: contextHash, RawBlockHash: digest(raw), OverlayHash: hash,
		BlockGasLeft: base.GasLimit - header.GasUsed,
	}
	return s, proof, nil
}
