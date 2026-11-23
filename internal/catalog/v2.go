package catalog

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

func verifyV2Reserves(ctx context.Context, reader execution.StateReader, address, token0, token1 domain.Address, fee uint32) (bootstrap.Spec, bool, error) {
	raw, err := reader.ReadContract(ctx, address, selector("getReserves()"))
	if err != nil {
		return bootstrap.Spec{}, false, err
	}
	if len(raw) != 96 {
		return bootstrap.Spec{}, false, fmt.Errorf("catalog: invalid V2 reserves ABI")
	}
	a, b, stamp := new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:64]), new(big.Int).SetBytes(raw[64:])
	if a.BitLen() > 112 || b.BitLen() > 112 || stamp.BitLen() > 32 {
		return bootstrap.Spec{}, false, fmt.Errorf("catalog: invalid V2 reserve widths")
	}
	return bootstrap.Spec{Address: address, Kind: domain.PoolKindV2, Token0: token0, Token1: token1, Fee: fee}, a.Sign() > 0 && b.Sign() > 0, nil
}
