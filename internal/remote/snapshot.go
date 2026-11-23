// Package remote implements L2 pool synchronization and execution over RPC.
// It has no dependency on Nitro or another embedded execution client.
package remote

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

type snapshot struct {
	risks          *tokenrisk.Registry
	riskBlock      domain.BlockRef
	paperFeeState  *snapshot
	client         *rpc.Client
	block          rpc.ExecutionBlock
	chain          domain.ChainIdentity
	deployments    domain.Deployments
	closed         atomic.Bool
	reads          atomic.Pointer[rpc.ReadCache]
	anchor         *rpc.ExecutionBlock
	overlay        map[string]any
	blockOverrides map[string]any
	// keepIdentity reuses immutable identity reads across blocks
	// (immutableReads); only the per-block canonical fee reader sets it.
	keepIdentity bool
}

// OpenSnapshot selects one canonical execution position. Subsequent reads use
// EIP-1898 block hashes, never a moving latest/pending block tag.
func OpenSnapshot(ctx context.Context, client *rpc.Client, chain domain.ChainIdentity, hash domain.Hash, deployments domain.Deployments) (execution.Snapshot, error) {
	if chain.ChainID == 0 || (chain.Family != domain.FamilyNitro && chain.Family != domain.FamilyOPStack) || hash == (domain.Hash{}) {
		return nil, fmt.Errorf("remote snapshot requires a supported L2 identity and block hash")
	}
	var text string
	if err := client.Call(ctx, "eth_chainId", []any{}, &text); err != nil {
		return nil, err
	}
	id, err := quantity(text)
	if err != nil || id != chain.ChainID {
		return nil, fmt.Errorf("remote snapshot chain mismatch")
	}
	block, err := client.ExecutionBlock(ctx, hash)
	if err != nil {
		return nil, err
	}
	if block.Ref.Hash != hash {
		return nil, fmt.Errorf("remote snapshot hash mismatch")
	}
	current, err := client.ExecutionBlock(ctx, fmt.Sprintf("0x%x", block.Ref.Number))
	if err != nil || current.Ref.Hash != hash {
		return nil, fmt.Errorf("remote snapshot is not canonical")
	}
	return &snapshot{client: client, block: block, chain: chain, deployments: deployments}, nil
}

func (s *snapshot) Position() execution.Position {
	return execution.Position{Chain: s.chain, Block: s.block.Ref, Timestamp: s.block.Ref.Timestamp, BaseFee: new(big.Int).Set(s.block.BaseFee), GasLimit: s.block.GasLimit}
}
func (s *snapshot) Retain() execution.Snapshot {
	retained := &snapshot{client: s.client, block: s.block, chain: s.chain, deployments: s.deployments, anchor: s.anchor, overlay: s.overlay, blockOverrides: s.blockOverrides}
	retained.reads.Store(s.readCache())
	return retained
}
func (s *snapshot) Close() { s.closed.Store(true) }
func (s *snapshot) FeeModel() execution.FeeModel {
	if s.chain.Family == domain.FamilyOPStack {
		return execution.OPFees{}
	}
	return execution.NitroRPCFees{}
}
func (s *snapshot) guard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return fmt.Errorf("remote snapshot closed")
	}
	return nil
}
func (s *snapshot) blockID() any {
	hash := s.block.Ref.Hash
	if s.anchor != nil {
		hash = s.anchor.Ref.Hash
	}
	return map[string]any{"blockHash": hash.Hex(), "requireCanonical": true}
}

func (s *snapshot) ReadContract(ctx context.Context, to domain.Address, input []byte) ([]byte, error) {
	return s.contractCall(ctx, to, input, nil)
}
func (s *snapshot) CodeHash(ctx context.Context, to domain.Address) (domain.Hash, error) {
	cacheable := s.keepIdentity && s.overlay == nil && s.chain.ChainID != 0
	if cacheable {
		if hash, ok := immutable.code(s.chain.ChainID, to); ok {
			return hash, s.guard(ctx)
		}
	}
	code, err := s.Code(ctx, to)
	if err != nil || len(code) == 0 {
		return domain.Hash{}, err
	}
	hash := digest(code)
	if cacheable {
		immutable.keepCode(s.chain.ChainID, to, hash)
	}
	return hash, nil
}

// Code returns the deployed code at the snapshot, including overlay code.
func (s *snapshot) Code(ctx context.Context, to domain.Address) ([]byte, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if account, ok := s.overlay[to.Hex()].(map[string]any); ok {
		if code, ok := account["code"].(string); ok {
			return hexData(code)
		}
	}
	output, err := s.readString(ctx, "eth_getCode", []any{to.Hex(), s.blockID()})
	if err != nil {
		return nil, err
	}
	return hexData(output)
}
func (s *snapshot) Nonce(ctx context.Context, to domain.Address) (uint64, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	if account, ok := s.overlay[to.Hex()].(map[string]any); ok {
		if nonce, ok := account["nonce"].(string); ok {
			return quantity(nonce)
		}
	}
	output, err := s.readString(ctx, "eth_getTransactionCount", []any{to.Hex(), s.blockID()})
	if err != nil {
		return 0, err
	}
	return quantity(output)
}
func (s *snapshot) balance(ctx context.Context, to domain.Address) (*big.Int, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if account, ok := s.overlay[to.Hex()].(map[string]any); ok {
		if balance, ok := account["balance"].(string); ok {
			return integer(balance)
		}
	}
	output, err := s.readString(ctx, "eth_getBalance", []any{to.Hex(), s.blockID()})
	if err != nil {
		return nil, err
	}
	return integer(output)
}
func (s *snapshot) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	if block != "0x"+strconv.FormatUint(s.block.Ref.Number, 16) {
		return fmt.Errorf("remote loader requested another snapshot")
	}
	value, err := s.ReadContract(ctx, to, data)
	if err == nil {
		*out = "0x" + hex.EncodeToString(value)
	}
	return err
}
func (s *snapshot) V4Deployment() (domain.Address, domain.Address) {
	return s.deployments.V4PoolManager, s.deployments.V4StateView
}
func (s *snapshot) Confirm(ctx context.Context, request execution.ConfirmationRequest) (*execution.ExecutionEvidence, error) {
	return s.confirm(ctx, request)
}

func digest(data []byte) domain.Hash {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	var hash domain.Hash
	copy(hash[:], h.Sum(nil))
	return hash
}
func selector(signature string) []byte { hash := digest([]byte(signature)); return hash[:4] }
func hexData(value string) ([]byte, error) {
	raw, ok := strings.CutPrefix(value, "0x")
	if !ok {
		return nil, fmt.Errorf("remote: missing hex prefix")
	}
	return hex.DecodeString(raw)
}
func quantity(value string) (uint64, error) {
	raw, ok := strings.CutPrefix(value, "0x")
	if !ok || raw == "" {
		return 0, fmt.Errorf("remote: invalid quantity")
	}
	return strconv.ParseUint(raw, 16, 64)
}
func integer(value string) (*big.Int, error) {
	if !strings.HasPrefix(value, "0x") {
		return nil, fmt.Errorf("remote: invalid integer")
	}
	n, ok := new(big.Int).SetString(value[2:], 16)
	if !ok || n.Sign() < 0 || n.BitLen() > 256 {
		return nil, fmt.Errorf("remote: invalid uint256")
	}
	return n, nil
}

var _ execution.Snapshot = (*snapshot)(nil)

func (s *snapshot) checkCanonical(ctx context.Context) error {
	expected := s.block
	if s.anchor != nil {
		expected = *s.anchor
	}
	current, err := s.client.ExecutionBlock(ctx, fmt.Sprintf("0x%x", expected.Ref.Number))
	if err != nil {
		return err
	}
	if current.Ref.Hash != expected.Ref.Hash {
		return fmt.Errorf("remote confirmation anchor is no longer canonical")
	}
	return nil
}

// FeeStateBlock binds local dynamic fee seeding to this exact execution state.
func (s *snapshot) FeeStateBlock() domain.BlockRef { return s.block.Ref }
