package slipstream

import (
	"context"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// The two retained OP factories use the pre-hook module. Their factory
// immutables also bind the corresponding pool implementations.
var legacyModules = map[string]string{
	"0x54e70bfdb89910349654db2f06c4a5cdbb9f4c74c781fe59276c1fa7b3f7f95e": "0xee7231de050995eec1137a289b89363626996ff4c62d4a7b8b912b1dea1831e1",
	"0xe6ce7d505ef62fec70e5fd128e3449c19a7cbc6acc2e29a5236fb8bded8988d2": "0xc3607d1b8b3cb103b6e9e6a6bcfb52cfba7b78d87265ced8051ced7bae27b7db",
}

func loadLegacy(ctx context.Context, c LocalStateCaller, p *pool.State, factory domain.Address, moduleHash, block string) error {
	ref := c.FeeStateBlock()
	m := &pool.SlipstreamState{Legacy: true, Factory: factory, Block: ref.Number, Timestamp: uint32(ref.Timestamp)}
	p.Slipstream = m
	module, err := readWords(ctx, c, factory, "swapFeeModule()", nil, block, 1)
	if err != nil {
		return err
	}
	m.Hook = wordAddress(module[0])
	hash, err := c.CodeHash(ctx, m.Hook)
	if err != nil {
		return err
	}
	if hash.Hex() != moduleHash {
		m.UnavailableReason = "unsupported_legacy_module_code"
		return nil
	}
	slot, err := readWords(ctx, c, p.Key.Address, "slot0()", nil, block, 6)
	if err != nil {
		return err
	}
	p.Tick = int32(decodeSignedWord(slot[1], 24))
	p.SqrtPriceX96 = new(big.Int).SetBytes(slot[0])
	m.ObservationIndex = uint16(wordUint(slot[2]))
	m.Cardinality = uint16(wordUint(slot[3]))
	m.CardinalityNext = uint16(wordUint(slot[4]))
	config, err := readWords(ctx, c, m.Hook, "dynamicFeeConfig(address)", addressWord(p.Key.Address), block, 3)
	if err != nil {
		return err
	}
	m.BaseFee = uint32(wordUint(config[0]))
	m.FeeCap = uint32(wordUint(config[1]))
	m.ScalingFactor = wordUint(config[2])
	defaults, err := readWords(ctx, c, m.Hook, "defaultScalingFactor()", nil, block, 1)
	if err != nil {
		return err
	}
	m.DefaultScalingFactor = wordUint(defaults[0])
	defaults, err = readWords(ctx, c, m.Hook, "defaultFeeCap()", nil, block, 1)
	if err != nil {
		return err
	}
	m.DefaultFeeCap = uint32(wordUint(defaults[0]))
	defaults, err = readWords(ctx, c, m.Hook, "secondsAgo()", nil, block, 1)
	if err != nil {
		return err
	}
	m.SecondsAgo = uint32(wordUint(defaults[0]))
	spacing, err := readWords(ctx, c, factory, "tickSpacingToFee(int24)", uintWord(uint64(p.TickSpacing)), block, 1)
	if err != nil {
		return err
	}
	m.SpacingFee = uint32(wordUint(spacing[0]))
	if err = loadOracle(ctx, c, p.Key.Address, m, block); err != nil {
		return err
	}
	m.Valid = true
	if err = ValidateState(m); err != nil {
		return err
	}
	fee, err := LocalFee(*p)
	if err != nil {
		return err
	}
	if fee != p.Fee {
		m.Valid = false
		m.UnavailableReason = "legacy_local_canonical_fee_mismatch"
	}
	return nil
}
