package remote

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// Setup executes the actual constructor and admin methods in private overlays.
// No guessed Solidity slots or mutations to token balances are necessary.
func (s *snapshot) setupPaperExecutor(ctx context.Context, artifact paperArtifact, sender domain.Address) (domain.Address, error) {
	// The paper sender is the owner and the first executeHot sender, so hot
	// routes simulate as the configured sender would send them.
	owner, err := artifact.abi.Pack("", common.Address(sender), [2]common.Address{common.Address(sender)})
	if err != nil {
		return domain.Address{}, err
	}
	s.overlay = mergeOverrides(s.overlay, map[string]any{sender.Hex(): map[string]any{"balance": "0x10000000000000000000000000", "nonce": "0x0", "code": "0x", "state": map[string]string{}}})
	args := s.paperSetupArgs(sender, domain.Address{}, append(append([]byte{}, artifact.code...), owner...), new(big.Int))
	var diff stateDiff
	if err := s.traceCall(ctx, args, map[string]any{"tracer": "prestateTracer", "tracerConfig": map[string]any{"diffMode": true}, "timeout": "2s"}, &diff); err != nil {
		return domain.Address{}, fmt.Errorf("paper constructor: %w", err)
	}
	patch, err := diff.overrides()
	if err != nil {
		return domain.Address{}, err
	}
	var executor domain.Address
	for address, raw := range patch {
		account := raw.(map[string]any)
		code, _ := account["code"].(string)
		if code == "" || code == "0x" {
			continue
		}
		if executor != (domain.Address{}) {
			return domain.Address{}, fmt.Errorf("constructor created multiple code accounts")
		}
		executor, err = domain.ParseAddress(address)
		if err != nil {
			return domain.Address{}, err
		}
	}
	if executor == (domain.Address{}) {
		return executor, fmt.Errorf("paper constructor produced no runtime code")
	}
	// An override must never silently replace an existing account on the chain.
	existing, err := s.CodeHash(ctx, executor)
	if err != nil || existing != (domain.Hash{}) {
		return executor, fmt.Errorf("paper executor address is occupied or unavailable")
	}
	s.overlay = mergeOverrides(s.overlay, patch)
	data, err := artifact.abi.Pack("setPaused", false)
	if err != nil {
		return executor, err
	}
	if err := s.paperSetupCall(ctx, sender, executor, data, new(big.Int)); err != nil {
		return executor, err
	}
	// The constructor starts at version 1 and setPaused moves it once.
	actual, err := s.readWord(ctx, executor, "configVersion()")
	if err != nil || !actual.IsUint64() || actual.Uint64() != 2 {
		return executor, fmt.Errorf("paper admin initialization reverted or config mismatch")
	}
	return executor, nil
}

func (s *snapshot) paperSetupArgs(sender, to domain.Address, data []byte, value *big.Int) map[string]any {
	args := map[string]any{"from": sender.Hex(), "type": "0x2", "chainId": fmt.Sprintf("0x%x", s.chain.ChainID), "data": flashHex(data), "gas": "0xf00000", "value": "0x" + value.Text(16), "maxFeePerGas": "0x" + new(big.Int).Mul(s.block.BaseFee, big.NewInt(2)).Text(16), "maxPriorityFeePerGas": "0x0"}
	if to != (domain.Address{}) {
		args["to"] = to.Hex()
	}
	return args
}
func (s *snapshot) paperSetupCall(ctx context.Context, sender, to domain.Address, data []byte, value *big.Int) error {
	overlay, err := s.postState(ctx, s.paperSetupArgs(sender, to, data, value), nil)
	if err != nil {
		return fmt.Errorf("paper setup call: %w", err)
	}
	s.overlay = overlay
	return nil
}

func (s *snapshot) fundPaperWallet(ctx context.Context, sender, executor, wrapped domain.Address, plan execution.ExecutorPlan) error {
	if plan.Funding != execution.FundingWallet || plan.Asset == (domain.Address{}) {
		return nil
	}
	if plan.Asset != wrapped {
		return fmt.Errorf("paper wallet funding only supports native or wrapped-native deposits")
	}
	if err := s.paperSetupCall(ctx, sender, wrapped, selector("deposit()"), plan.Amount); err != nil {
		return err
	}
	data := make([]byte, 68)
	copy(data, selector("approve(address,uint256)"))
	copy(data[16:36], executor[:])
	plan.Amount.FillBytes(data[36:])
	return s.paperSetupCall(ctx, sender, wrapped, data, new(big.Int))
}
