package state

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

func validateVelodromeWire(p poolWire, version int) error {
	isVelodrome := domain.PoolKind(p.Kind) == domain.PoolKindVelodromeV2
	if p.Velodrome == nil && !isVelodrome {
		return nil
	}
	if p.Velodrome == nil || !isVelodrome || version < ProtocolStateVersion {
		return fmt.Errorf("%w: Velodrome metadata requires matching kind and version 3", ErrInvalidSnapshot)
	}
	m := p.Velodrome
	factory, err := domain.ParseAddress(m.Factory)
	if err != nil || factory == (domain.Address{}) {
		return fmt.Errorf("%w: Velodrome factory", ErrInvalidSnapshot)
	}
	for _, text := range []string{m.Scale0, m.Scale1} {
		v, ok := new(big.Int).SetString(text, 10)
		if !ok || v.Sign() <= 0 || v.BitLen() > 256 {
			return fmt.Errorf("%w: Velodrome decimal scale", ErrInvalidSnapshot)
		}
		remainder := new(big.Int)
		for v.Cmp(big.NewInt(1)) > 0 {
			v.QuoRem(v, big.NewInt(10), remainder)
			if remainder.Sign() != 0 {
				return fmt.Errorf("%w: Velodrome scale is not a power of ten", ErrInvalidSnapshot)
			}
		}
	}
	if p.Fee%100 != 0 || p.Fee > 10000 {
		return fmt.Errorf("%w: Velodrome factory fee", ErrInvalidSnapshot)
	}
	return nil
}
