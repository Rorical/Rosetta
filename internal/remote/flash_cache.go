package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/rpc"
)

const flashCacheMethod = "rosetta_getFlashblocksStateV1"

// flashSnapshotSource prefers the node's already executed state. This method
// is an explicit node extension, not part of standard Base JSON-RPC. Only a
// method-not-found response permits replay fallback; stale/bad exports fail.
type flashSnapshotSource struct{ unsupported atomic.Bool }

func (s *flashSnapshotSource) open(ctx context.Context, client *rpc.Client, anchor rpc.ExecutionBlock, chain domain.ChainIdentity, deployments domain.Deployments, prefix flashPrefix) (*snapshot, *execution.FlashblocksEvidence, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("Flashblocks snapshot source is not configured")
	}
	if s.unsupported.Load() {
		return openFlashSnapshot(ctx, client, anchor, chain, deployments, prefix)
	}
	view, proof, err := openCachedFlashSnapshot(ctx, client, anchor, chain, deployments, prefix)
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		return view, proof, err
	}
	if !s.unsupported.Swap(true) {
		slog.Info("Flashblocks node cache export unavailable; using native prefix replay")
	}
	return openFlashSnapshot(ctx, client, anchor, chain, deployments, prefix)
}

// cachedFlashState must come from one atomic immutable node cache handle.
// State overrides are complete changes relative to ParentHash, including OP
// fees, deposits and system writes. An empty override object is meaningful.
type cachedFlashState struct {
	ChainID         string                     `json:"chainId"`
	PayloadID       string                     `json:"payloadId"`
	Index           *uint64                    `json:"index"`
	ParentHash      string                     `json:"parentHash"`
	BlockNumber     string                     `json:"blockNumber"`
	Timestamp       string                     `json:"timestamp"`
	GasLimit        string                     `json:"gasLimit"`
	GasUsed         string                     `json:"gasUsed"`
	BaseFee         string                     `json:"baseFeePerGas"`
	Coinbase        string                     `json:"feeRecipient"`
	Random          string                     `json:"prevRandao"`
	BeaconRoot      string                     `json:"parentBeaconBlockRoot"`
	Extra           string                     `json:"extraData"`
	WithdrawalsRoot string                     `json:"withdrawalsRoot"`
	BlobGasUsed     *string                    `json:"blobGasUsed,omitempty"`
	Receipts        []json.RawMessage          `json:"receipts"`
	StateOverrides  map[string]json.RawMessage `json:"stateOverrides"`
}

func openCachedFlashSnapshot(ctx context.Context, client *rpc.Client, anchor rpc.ExecutionBlock, chain domain.ChainIdentity, deployments domain.Deployments, prefix flashPrefix) (*snapshot, *execution.FlashblocksEvidence, error) {
	if chain.Family != domain.FamilyOPStack || prefix.Version.Anchor != anchor.Ref || prefix.Version.Source != domain.SourceFast {
		return nil, nil, fmt.Errorf("Flashblocks cached state anchor mismatch")
	}
	raw, header, err := prefix.rawBlock()
	if err != nil {
		return nil, nil, err
	}
	// The selector is a requirement, not a request to stamp/echo an identity
	// onto the node's current pending state. A missing prefix must return error.
	selector := map[string]any{"chainId": fmt.Sprintf("0x%x", chain.ChainID), "parentHash": anchor.Ref.Hash.Hex(), "payloadId": prefix.Payload, "index": prefix.Index}
	var cached *cachedFlashState
	if err := client.Call(ctx, flashCacheMethod, []any{selector}, &cached); err != nil {
		return nil, nil, fmt.Errorf("Flashblocks cache export: %w", err)
	}
	if cached == nil {
		return nil, nil, fmt.Errorf("Flashblocks cache prefix unavailable")
	}
	if err := cached.matches(chain.ChainID, prefix); err != nil {
		return nil, nil, err
	}
	overlay, err := decodeCachedOverrides(cached.StateOverrides)
	if err != nil {
		return nil, nil, err
	}
	view, proof, err := bindFlashSnapshot(client, anchor, chain, deployments, prefix, raw, header, overlay)
	if err != nil {
		return nil, nil, err
	}
	if err := view.checkCanonical(ctx); err != nil {
		return nil, nil, err
	}
	proof.StateSource = "rpc-cache-v1"
	return view, proof, nil
}
