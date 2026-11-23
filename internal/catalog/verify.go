package catalog

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
)

type poolTier struct {
	fee     uint32
	spacing int32
}

func factoryTiers(ctx context.Context, reader execution.StateReader, factory Factory) ([]poolTier, error) {
	var result []poolTier
	if factory.Kind == domain.PoolKindVelodromeV2 {
		return []poolTier{{fee: 0}, {fee: 1}}, nil
	}
	if factory.Kind == domain.PoolKindV2 {
		return []poolTier{{fee: factory.Fees[0]}}, nil
	}
	if factory.Kind == domain.PoolKindV3 {
		for _, fee := range factory.Fees {
			data := make([]byte, 36)
			copy(data, selector("feeAmountTickSpacing(uint24)"))
			new(big.Int).SetUint64(uint64(fee)).FillBytes(data[4:])
			spacing, err := readWord(ctx, reader, factory.Address, data)
			if err != nil {
				return nil, err
			}
			if spacing.Sign() == 0 {
				continue
			}
			if !spacing.IsUint64() || spacing.Uint64() > 32767 {
				return nil, fmt.Errorf("catalog: invalid V3 tick spacing")
			}
			result = append(result, poolTier{fee: fee, spacing: int32(spacing.Uint64())})
		}
		return result, nil
	}
	raw, err := reader.ReadContract(ctx, factory.Address, selector("tickSpacings()"))
	if err != nil {
		return nil, err
	}
	if len(raw) < 64 || !bytes.Equal(raw[:32], word(32)) {
		return nil, fmt.Errorf("catalog: invalid tick spacings ABI")
	}
	count := new(big.Int).SetBytes(raw[32:64])
	if !count.IsUint64() || count.Uint64() > 64 || len(raw) != 64+32*int(count.Uint64()) {
		return nil, fmt.Errorf("catalog: invalid tick spacing count")
	}
	seen := make(map[int32]bool)
	for i := 64; i < len(raw); i += 32 {
		spacing := new(big.Int).SetBytes(raw[i : i+32])
		if !spacing.IsUint64() || spacing.Sign() == 0 || spacing.Uint64() > 32767 || seen[int32(spacing.Uint64())] {
			return nil, fmt.Errorf("catalog: invalid Slipstream tick spacing")
		}
		value := int32(spacing.Uint64())
		seen[value] = true
		result = append(result, poolTier{spacing: value})
	}
	slices.SortFunc(result, func(a, b poolTier) int { return int(a.spacing - b.spacing) })
	return result, nil
}
func verifyPool(ctx context.Context, reader execution.StateReader, factory Factory, address, token0, token1 domain.Address, tier poolTier) (bootstrap.Spec, bool, error) {
	fail := func(err error) (bootstrap.Spec, bool, error) {
		return bootstrap.Spec{}, false, fmt.Errorf("catalog pool %s: %w", address.Hex(), err)
	}
	if err := checkCode(ctx, reader, address); err != nil {
		return fail(err)
	}
	for _, entry := range []struct {
		signature string
		expected  domain.Address
	}{{"factory()", factory.Address}, {"token0()", token0}, {"token1()", token1}} {
		if entry.signature == "factory()" && factory.Address == velodromev2.LegacyFactory() {
			continue // V1 pairs have no getter; the factory lookup above vouches for them
		}
		value, err := readAddress(ctx, reader, address, selector(entry.signature))
		if err != nil {
			return fail(err)
		}
		if value != entry.expected {
			return fail(fmt.Errorf("%s differs from factory lookup", entry.signature))
		}
	}
	if factory.Kind == domain.PoolKindV2 {
		return verifyV2Reserves(ctx, reader, address, token0, token1, tier.fee)
	}
	if factory.Kind == domain.PoolKindVelodromeV2 {
		return verifyVelodrome(ctx, reader, address, token0, token1)
	}
	if factory.Kind == domain.PoolKindCamelotV3 {
		return verifyCamelotMetadata(ctx, reader, address, token0, token1)
	}
	if factory.Kind == domain.PoolKindSolidlyV3 {
		return verifySolidly(ctx, reader, address, token0, token1, tier.spacing)
	}
	spacing, err := readWord(ctx, reader, address, selector("tickSpacing()"))
	if err != nil {
		return fail(err)
	}
	if !spacing.IsInt64() || spacing.Int64() != int64(tier.spacing) {
		return fail(fmt.Errorf("pool tick spacing mismatch"))
	}
	fee, err := readWord(ctx, reader, address, selector("fee()"))
	if err != nil {
		return fail(err)
	}
	if !fee.IsUint64() || fee.Uint64() >= 1_000_000 || factory.Kind == domain.PoolKindV3 && fee.Uint64() != uint64(tier.fee) {
		return fail(fmt.Errorf("pool fee mismatch"))
	}
	liquidity, err := readWord(ctx, reader, address, selector("liquidity()"))
	if err != nil {
		return fail(err)
	}
	if liquidity.BitLen() > 128 {
		return fail(fmt.Errorf("invalid liquidity"))
	}
	return bootstrap.Spec{Address: address, Kind: factory.Kind, Token0: token0, Token1: token1, Fee: uint32(fee.Uint64()), TickSpacing: tier.spacing}, liquidity.Sign() > 0, nil
}

// verifySolidly reads the tick spacing and the slot0 fee: Solidly V3 has no
// fee() getter and its fee can differ from the creation tier after SetFee.
func verifySolidly(ctx context.Context, reader execution.StateReader, address, token0, token1 domain.Address, spacing int32) (bootstrap.Spec, bool, error) {
	fail := func(err error) (bootstrap.Spec, bool, error) {
		return bootstrap.Spec{}, false, fmt.Errorf("catalog pool %s: %w", address.Hex(), err)
	}
	actual, err := readWord(ctx, reader, address, selector("tickSpacing()"))
	if err != nil {
		return fail(err)
	}
	if !actual.IsInt64() || actual.Int64() != int64(spacing) || spacing <= 0 {
		return fail(fmt.Errorf("pool tick spacing mismatch"))
	}
	slot0, err := reader.ReadContract(ctx, address, selector("slot0()"))
	if err != nil {
		return fail(err)
	}
	if len(slot0) < 128 {
		return fail(fmt.Errorf("invalid Solidly V3 slot0"))
	}
	fee := new(big.Int).SetBytes(slot0[64:96])
	if !fee.IsUint64() || fee.Uint64() > 100_000 {
		return fail(fmt.Errorf("Solidly V3 fee above the pool cap"))
	}
	liquidity, err := readWord(ctx, reader, address, selector("liquidity()"))
	if err != nil {
		return fail(err)
	}
	if liquidity.BitLen() > 128 {
		return fail(fmt.Errorf("invalid liquidity"))
	}
	return bootstrap.Spec{Address: address, Kind: domain.PoolKindSolidlyV3, Token0: token0, Token1: token1, Fee: uint32(fee.Uint64()), TickSpacing: spacing}, liquidity.Sign() > 0, nil
}

func word(value uint64) []byte { return new(big.Int).SetUint64(value).FillBytes(make([]byte, 32)) }

func resolvePair(ctx context.Context, reader execution.StateReader, factory Factory, token0, token1 domain.Address, tiers []poolTier, seen map[domain.Address]bool) ([]bootstrap.Spec, error) {
	var pools []bootstrap.Spec
	for _, tier := range tiers {
		input := callData("getPool(address,address,uint24)", token0, token1, uint64(tier.fee))
		if factory.Kind == domain.PoolKindVelodromeV2 {
			input = callData("getPool(address,address,bool)", token0, token1, uint64(tier.fee))
		}
		if factory.Kind == domain.PoolKindV2 {
			input = callData("getPair(address,address)", token0, token1, 0)[:68]
		}
		if factory.Kind == domain.PoolKindSlipstream {
			input = callData("getPool(address,address,int24)", token0, token1, uint64(tier.spacing))
		}
		address, err := readAddress(ctx, reader, factory.Address, input)
		if err != nil {
			return nil, err
		}
		if address == (domain.Address{}) {
			continue
		}
		if seen[address] {
			return nil, fmt.Errorf("catalog: factory returned duplicate pool %s", address.Hex())
		}
		seen[address] = true
		spec, active, err := verifyPool(ctx, reader, factory, address, token0, token1, tier)
		if err != nil {
			return nil, err
		}
		if active {
			pools = append(pools, spec)
		}
	}
	return pools, nil
}

// VerifyPool checks creation metadata against the configured factory at a fixed reader.
func VerifyPool(ctx context.Context, reader execution.StateReader, factory Factory, spec bootstrap.Spec) (bootstrap.Spec, bool, error) {
	if spec.Kind != factory.Kind {
		return bootstrap.Spec{}, false, fmt.Errorf("catalog: creation kind differs from factory")
	}
	input := callData("getPool(address,address,uint24)", spec.Token0, spec.Token1, uint64(spec.Fee))
	if factory.Kind == domain.PoolKindVelodromeV2 {
		stable, err := readWord(ctx, reader, spec.Address, selector("stable()"))
		if err != nil {
			return spec, false, err
		}
		if stable.BitLen() > 1 {
			return spec, false, fmt.Errorf("catalog: invalid stable flag")
		}
		input = callData("getPool(address,address,bool)", spec.Token0, spec.Token1, stable.Uint64())
		if factory.Address == velodromev2.LegacyFactory() {
			input = callData("getPair(address,address,bool)", spec.Token0, spec.Token1, stable.Uint64())
		}
	}
	if factory.Kind == domain.PoolKindV2 {
		input = callData("getPair(address,address)", spec.Token0, spec.Token1, 0)[:68]
	}
	if factory.Kind == domain.PoolKindSlipstream || factory.Kind == domain.PoolKindSolidlyV3 {
		input = callData("getPool(address,address,int24)", spec.Token0, spec.Token1, uint64(spec.TickSpacing))
	}
	if factory.Kind == domain.PoolKindCamelotV3 {
		input = callData("poolByPair(address,address)", spec.Token0, spec.Token1, 0)[:68]
	}
	actual, err := readAddress(ctx, reader, factory.Address, input)
	if err != nil {
		return spec, false, err
	}
	if actual != spec.Address {
		return spec, false, fmt.Errorf("%w: catalog candidate is not registered by factory", pool.ErrCallRejected)
	}
	return verifyPool(ctx, reader, factory, spec.Address, spec.Token0, spec.Token1, poolTier{fee: spec.Fee, spacing: spec.TickSpacing})
}
