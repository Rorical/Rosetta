package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type paperCanonicalBlock struct {
	header       *flashHeader
	ref          domain.BlockRef
	transactions []domain.Hash
}
type paperCallTrace struct {
	Hash   string         `json:"txHash"`
	Error  string         `json:"error"`
	Result flashCallFrame `json:"result"`
}
type paperDiffTrace struct {
	Hash   string    `json:"txHash"`
	Error  string    `json:"error"`
	Result stateDiff `json:"result"`
}

// This cache belongs exclusively to the serial paper worker. It retains one
// immutable canonical block, shared by the group's original and later prefixes.
type paperCanonicalSource struct {
	receipts     []rpc.ExecutionReceipt
	receiptError error
	block        paperCanonicalBlock
	calls        []paperCallTrace
	diffs        []paperDiffTrace
}

func readPaperCanonicalBlock(ctx context.Context, client *rpc.Client, number uint64) (paperCanonicalBlock, error) {
	var raw json.RawMessage
	if err := client.Call(ctx, "eth_getBlockByNumber", []any{fmt.Sprintf("0x%x", number), false}, &raw); err != nil {
		return paperCanonicalBlock{}, err
	}
	if bytes.Equal(raw, []byte("null")) {
		return paperCanonicalBlock{}, rpc.ErrBlockUnavailable
	}

	var body struct {
		Hash, Number, ParentHash, Timestamp, GasLimit, BaseFeePerGas, Miner, MixHash string
		ParentBeaconBlockRoot, ExtraData                                             *string
		Transactions                                                                 []string
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return paperCanonicalBlock{}, err
	}
	hash, err := domain.ParseHash(body.Hash)
	height, heightErr := quantity(body.Number)
	timestamp, timeErr := quantity(body.Timestamp)
	gas, gasErr := quantity(body.GasLimit)
	fee, feeOK := new(big.Int).SetString(body.BaseFeePerGas, 0)
	parent, parentErr := domain.ParseHash(body.ParentHash)
	miner, minerErr := domain.ParseAddress(body.Miner)
	random, randomErr := domain.ParseHash(body.MixHash)
	if err != nil || hash == (domain.Hash{}) || heightErr != nil || height != number || timeErr != nil || gasErr != nil || gas == 0 || !feeOK || fee.Sign() <= 0 || parentErr != nil || minerErr != nil || randomErr != nil || body.ParentBeaconBlockRoot == nil || body.ExtraData == nil {
		return paperCanonicalBlock{}, fmt.Errorf("paper canonical header incomplete or inconsistent")
	}
	beacon, err := domain.ParseHash(*body.ParentBeaconBlockRoot)
	if err != nil {
		return paperCanonicalBlock{}, err
	}
	extra, err := hexData(*body.ExtraData)
	if err != nil {
		return paperCanonicalBlock{}, err
	}
	if len(body.Transactions) > 4096 {
		return paperCanonicalBlock{}, fmt.Errorf("paper canonical block exceeds transaction bound")
	}
	header := &flashHeader{ParentHash: parent, Number: number, Time: timestamp, GasLimit: gas, BaseFee: fee, Coinbase: miner, MixDigest: random, ParentBeaconRoot: beacon, Extra: extra}
	block := paperCanonicalBlock{header: header, ref: domain.BlockRef{Number: number, Hash: hash, ParentHash: parent, Timestamp: timestamp}}
	for _, text := range body.Transactions {
		hash, err := domain.ParseHash(text)
		if err != nil {
			return paperCanonicalBlock{}, err
		}
		block.transactions = append(block.transactions, hash)
	}
	return block, nil
}
func verifyPaperCanonical(ctx context.Context, client *rpc.Client, ref domain.BlockRef) error {
	current, err := client.ExecutionBlock(ctx, fmt.Sprintf("0x%x", ref.Number))
	if err != nil {
		return err
	}
	if current.Ref.Hash != ref.Hash {
		return fmt.Errorf("paper trace block is no longer canonical")
	}
	return nil
}
func (c *paperCanonicalSource) load(ctx context.Context, client *rpc.Client, number uint64) error {
	var block paperCanonicalBlock
	for {
		var err error
		block, err = readPaperCanonicalBlock(ctx, client, number)
		if err == nil {
			break
		}
		if !errors.Is(err, rpc.ErrBlockUnavailable) {
			return err
		}
		timer := time.NewTimer(400 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if block.ref.Hash == c.block.ref.Hash {
		return nil
	}
	var calls []paperCallTrace
	var diffs []paperDiffTrace
	// Separate native tracers work on providers that prohibit muxTracer. Verify
	// identities and the canonical header before publishing their combined result.
	errorsOut := make(chan error, 2)
	tag := fmt.Sprintf("0x%x", number)
	go func() {
		errorsOut <- client.Call(ctx, "debug_traceBlockByNumber", []any{tag, map[string]any{"tracer": "callTracer", "tracerConfig": map[string]any{"withLog": true}, "timeout": "2s"}}, &calls)
	}()
	go func() {
		errorsOut <- client.Call(ctx, "debug_traceBlockByNumber", []any{tag, map[string]any{"tracer": "prestateTracer", "tracerConfig": map[string]any{"diffMode": true}, "timeout": "2s"}}, &diffs)
	}()
	if err := errors.Join(<-errorsOut, <-errorsOut); err != nil {
		return err
	}
	if len(calls) != len(block.transactions) || len(diffs) != len(block.transactions) {
		return fmt.Errorf("paper canonical traces incomplete")
	}
	for i, hash := range block.transactions {
		if calls[i].Hash != hash.Hex() || diffs[i].Hash != hash.Hex() || calls[i].Error != "" || diffs[i].Error != "" {
			return fmt.Errorf("paper canonical trace %d identity/error mismatch", i)
		}
	}
	if err := verifyPaperCanonical(ctx, client, block.ref); err != nil {
		return err
	}
	c.block, c.calls, c.diffs = block, calls, diffs
	c.receipts, c.receiptError = nil, nil
	return nil
}
func (c *paperCanonicalSource) open(ctx context.Context, client *rpc.Client, anchor rpc.ExecutionBlock, chain domain.ChainIdentity, deployments domain.Deployments, prefix flashPrefix) (*snapshot, *execution.FlashblocksEvidence, *domain.BlockRef, error) {
	if err := c.load(ctx, client, prefix.Base.Number); err != nil {
		return nil, nil, nil, err
	}
	if err := c.matches(prefix); err != nil {
		return nil, nil, nil, err
	}
	raw, header, err := prefix.rawBlock()
	if err != nil {
		return nil, nil, nil, err
	}
	parent := &snapshot{client: client, block: anchor, chain: chain, deployments: deployments}
	overlay, err := parent.flashSystemState(ctx, prefix)
	if err != nil {
		return nil, nil, nil, err
	}
	var gas uint64
	for i, tx := range prefix.Transactions {
		call := c.calls[i].Result
		if (call.Error == "") != tx.Successful {
			return nil, nil, nil, fmt.Errorf("paper prefix receipt status mismatch at %d", i)
		}
		used, err := quantity(call.GasUsed)
		if err != nil || used > header.GasLimit-gas {
			return nil, nil, nil, fmt.Errorf("paper prefix gas mismatch")
		}
		gas += used
		logs, err := call.logs()
		if err != nil {
			return nil, nil, nil, err
		}
		if !equalFlashLogs(logs, tx.Logs) {
			return nil, nil, nil, fmt.Errorf("paper prefix logs mismatch at %d", i)
		}
		patch, err := c.diffs[i].Result.overrides()
		if err != nil {
			return nil, nil, nil, err
		}
		applyOverridePatch(overlay, patch)
	}
	if gas > header.GasUsed {
		return nil, nil, nil, fmt.Errorf("paper prefix understates execution gas")
	}
	view, proof, err := bindFlashSnapshot(client, anchor, chain, deployments, prefix, raw, header, overlay)
	if err != nil {
		return nil, nil, nil, err
	}
	proof.StateSource = "canonical-block-traces"
	ref := c.block.ref
	return view, proof, &ref, nil
}
func (c *paperCanonicalSource) matches(prefix flashPrefix) error {
	h, e := c.block.header, prefix.Base.Execution
	if e == nil || e.BaseFee == nil || h.BaseFee == nil || prefix.Base.Parent != c.block.ref.ParentHash || prefix.Base.Number != c.block.ref.Number || prefix.Base.Timestamp != h.Time || e.GasLimit != h.GasLimit || e.BaseFee.Cmp(h.BaseFee) != 0 || e.Coinbase != domain.Address(h.Coinbase) || e.Random != domain.Hash(h.MixDigest) || e.BeaconRoot != h.ParentBeaconRoot || !bytes.Equal(e.Extra, h.Extra) {
		return fmt.Errorf("paper prefix execution context differs from canonical block")
	}
	if len(prefix.Transactions) > len(c.block.transactions) {
		return fmt.Errorf("paper prefix longer than canonical block")
	}
	for i, tx := range prefix.Transactions {
		if tx.Hash != c.block.transactions[i] {
			return fmt.Errorf("paper prefix is not a canonical transaction prefix")
		}
	}
	return nil
}
