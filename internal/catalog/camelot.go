package catalog

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// Camelot V3 has a pair lookup and mutable directional globalState fees. It
// cannot use the Uniswap getPool/fee ABI, even though its swap math is shared.
func verifyCamelotMetadata(ctx context.Context, reader execution.StateReader, address, token0, token1 domain.Address) (bootstrap.Spec, bool, error) {
	global, err := reader.ReadContract(ctx, address, selector("globalState()"))
	if err != nil {
		return bootstrap.Spec{}, false, err
	}
	spacing, err := reader.ReadContract(ctx, address, selector("tickSpacing()"))
	if err != nil {
		return bootstrap.Spec{}, false, err
	}
	metadata, err := camelotv3.ParseMetadata("0x"+hex.EncodeToString(global), "0x"+hex.EncodeToString(spacing))
	if err != nil {
		return bootstrap.Spec{}, false, err
	}
	liquidity, err := readWord(ctx, reader, address, selector("liquidity()"))
	if err != nil {
		return bootstrap.Spec{}, false, err
	}
	if liquidity.BitLen() > 128 {
		return bootstrap.Spec{}, false, fmt.Errorf("catalog: invalid Camelot liquidity")
	}
	return bootstrap.Spec{Address: address, Token0: token0, Token1: token1, Kind: domain.PoolKindCamelotV3, Fee: metadata.Fee, TickSpacing: metadata.TickSpacing}, liquidity.Sign() > 0, nil
}
