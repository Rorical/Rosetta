package catalog

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// A catalog reader already owns a fixed state position. Reuse the protocol
// loader so catalog admission and bootstrap cannot decode different metadata.
type snapshotCaller struct{ reader execution.StateReader }

func (c snapshotCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	if block != fmt.Sprintf("0x%x", c.reader.Position().Block.Number) {
		return fmt.Errorf("catalog loader requested another snapshot")
	}
	raw, err := c.reader.ReadContract(ctx, to, data)
	if err == nil {
		*out = "0x" + hex.EncodeToString(raw)
	}
	return err
}

func verifyVelodrome(ctx context.Context, reader execution.StateReader, address, token0, token1 domain.Address) (bootstrap.Spec, bool, error) {
	p, err := velodromev2.Load(ctx, snapshotCaller{reader}, address, token0, token1, fmt.Sprintf("0x%x", reader.Position().Block.Number))
	if err != nil {
		return bootstrap.Spec{}, false, err
	}
	return bootstrap.Spec{Address: address, Kind: p.Key.Kind, Token0: token0, Token1: token1, Fee: p.Fee, Velodrome: p.Velodrome}, p.Reserve0.Sign() > 0 && p.Reserve1.Sign() > 0, nil
}
