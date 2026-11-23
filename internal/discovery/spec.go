package discovery

import (
	"github.com/Rorical/Rosetta/internal/domain"
)

// Spec identifies one discovered pool without importing bootstrap. Creation
// metadata also retains the stable/volatile curve for factory fee reads.
//
// Address is the pool contract except for the V4 singleton. PoolID and Hooks
// are zero outside V4. Fee is in pips (1e-6), including V2. Camelot's reverse
// fee is checked during discovery and loaded into pool.State from globalState.
type Spec struct {
	Address     domain.Address
	Token0      domain.Address
	Token1      domain.Address
	Kind        domain.PoolKind
	PoolID      domain.Hash
	Hooks       domain.Address
	Fee         uint32
	TickSpacing int32
	// Velodrome creation events carry the immutable curve type and factory.
	Stable  bool
	Factory domain.Address
}

// PoolKey returns the stable store key of the pool the spec describes.
func (s Spec) PoolKey() domain.PoolKey {
	return domain.PoolKey{
		Address: s.Address,
		Kind:    s.Kind,
		PoolID:  s.PoolID,
		Hooks:   s.Hooks,
	}
}

// label renders the spec for an error or log line. V4 pools share the
// PoolManager address, so the pool ID is part of the name.
func (s Spec) label() string {
	if s.PoolID == (domain.Hash{}) {
		return s.Address.Hex()
	}
	return s.Address.Hex() + "/" + s.PoolID.Hex()
}
