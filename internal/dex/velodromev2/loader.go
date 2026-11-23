package velodromev2

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

var (
	factory, _ = domain.ParseAddress("0xf1046053aa5682b4f9a81b5481394da16be5ff5a")
	// legacyFactory is the Velodrome V1 PairFactory. Its pairs have no
	// factory() getter and take one fee per curve from getFee(bool).
	legacyFactory, _ = domain.ParseAddress("0x25cbddb98b35ab1ff77413456b31ec81a6b6b746")
)

// LegacyFactory is the Velodrome V1 PairFactory address.
func LegacyFactory() domain.Address { return legacyFactory }

// Load reads metadata and the effective factory fee at one fixed block. The
// supported factory has an immutable implementation; other versions need their
// own arithmetic fixtures before admission.
func Load(ctx context.Context, caller pool.ContractCaller, address, token0, token1 domain.Address, block string) (pool.State, error) {
	if caller == nil || address == (domain.Address{}) || token0 == token1 || block == "" {
		return pool.State{}, fmt.Errorf("%w: Velodrome load specification", pool.ErrInvalidState)
	}
	raw, err := call(ctx, caller, address, []byte{0x39, 0x2f, 0x37, 0xe9}, block, 7)
	if err != nil {
		return pool.State{}, err
	}
	if !validAddressWord(raw[160:192], token0) || !validAddressWord(raw[192:224], token1) {
		return pool.State{}, fmt.Errorf("%w: Velodrome metadata tokens differ", pool.ErrInvalidState)
	}
	stable := new(big.Int).SetBytes(raw[128:160])
	if stable.BitLen() > 1 {
		return pool.State{}, fmt.Errorf("%w: Velodrome stable flag", pool.ErrInvalidState)
	}
	m := &pool.VelodromeState{Factory: factory, Stable: stable.Sign() != 0, Scale0: new(big.Int).SetBytes(raw[:32]), Scale1: new(big.Int).SetBytes(raw[32:64])}
	if !decimalScale(m.Scale0) || !decimalScale(m.Scale1) {
		return pool.State{}, fmt.Errorf("%w: Velodrome decimal scales", pool.ErrInvalidState)
	}
	p := pool.State{Key: domain.PoolKey{Address: address, Kind: domain.PoolKindVelodromeV2}, Token0: token0, Token1: token1,
		Reserve0: new(big.Int).SetBytes(raw[64:96]), Reserve1: new(big.Int).SetBytes(raw[96:128]), Velodrome: m}
	if m.Factory, err = poolFactory(ctx, caller, address, block); err != nil {
		return pool.State{}, err
	}
	return LoadFee(ctx, caller, p, block)
}

// poolFactory resolves a pair to a supported factory: a V2 pool names its
// factory; a V1 pair has no getter and is confirmed by the V1 factory.
func poolFactory(ctx context.Context, caller pool.ContractCaller, address domain.Address, block string) (domain.Address, error) {
	raw, err := call(ctx, caller, address, []byte{0xc4, 0x5a, 0x01, 0x55}, block, 1)
	if err == nil {
		if !validAddressWord(raw, factory) {
			return domain.Address{}, fmt.Errorf("%w: unverified Velodrome factory", domain.ErrUnsupported)
		}
		return factory, nil
	}
	data := make([]byte, 36)
	copy(data, []byte{0xe5, 0xe3, 0x1b, 0x13}) // isPair(address)
	copy(data[16:], address[:])
	raw, legacyErr := call(ctx, caller, legacyFactory, data, block, 1)
	if legacyErr != nil || new(big.Int).SetBytes(raw).Cmp(big.NewInt(1)) != 0 {
		return domain.Address{}, fmt.Errorf("%w: unverified Velodrome factory: %v", domain.ErrUnsupported, err)
	}
	return legacyFactory, nil
}

// FeeCall is shared with canonical batching. Metadata is retained from the
// bootstrap; ordinary fee refreshes never repeat immutable metadata getters.
func FeeCall(p pool.State, block string) (pool.ContractCall, error) {
	if p.Velodrome == nil || !supportedFactory(p.Velodrome) {
		return pool.ContractCall{}, fmt.Errorf("%w: missing Velodrome fee metadata", pool.ErrInvalidState)
	}
	if p.Velodrome.Factory == legacyFactory {
		data := make([]byte, 36)
		copy(data, []byte{0x51, 0x2b, 0x45, 0xea}) // getFee(bool): one fee per curve
		if p.Velodrome.Stable {
			data[35] = 1
		}
		return pool.ContractCall{To: legacyFactory, Data: data, Block: block}, nil
	}
	data := make([]byte, 68)
	copy(data, []byte{0xcc, 0x56, 0xb2, 0xc5})
	copy(data[16:36], p.Key.Address[:])
	if p.Velodrome.Stable {
		data[67] = 1
	}
	return pool.ContractCall{To: p.Velodrome.Factory, Data: data, Block: block}, nil
}

func LoadFee(ctx context.Context, caller pool.ContractCaller, p pool.State, block string) (pool.State, error) {
	request, err := FeeCall(p, block)
	if err != nil {
		return pool.State{}, err
	}
	raw, err := call(ctx, caller, request.To, request.Data, block, 1)
	if err != nil {
		return pool.State{}, err
	}
	fee := new(big.Int).SetBytes(raw)
	if !fee.IsUint64() || fee.Uint64() > 100 {
		return pool.State{}, fmt.Errorf("%w: Velodrome fee outside factory limit", pool.ErrInvalidState)
	}
	p.Fee = uint32(fee.Uint64()) * 100
	return p, nil
}

func call(ctx context.Context, caller pool.ContractCaller, to domain.Address, data []byte, block string, words int) ([]byte, error) {
	var result string
	if err := caller.CallContract(ctx, to, data, block, &result); err != nil {
		return nil, fmt.Errorf("Velodrome call %x: %w", data[:4], err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(result, "0x"))
	if err != nil || len(raw) != words*32 {
		return nil, fmt.Errorf("%w: malformed Velodrome response to %x", pool.ErrInvalidState, data[:4])
	}
	return raw, nil
}
func validAddressWord(raw []byte, address domain.Address) bool {
	return new(big.Int).SetBytes(raw).Cmp(new(big.Int).SetBytes(address[:])) == 0
}
func decimalScale(value *big.Int) bool {
	if value.Sign() <= 0 {
		return false
	}
	v := new(big.Int).Set(value)
	ten, remainder := big.NewInt(10), new(big.Int)
	for v.Cmp(big.NewInt(1)) > 0 {
		v.QuoRem(v, ten, remainder)
		if remainder.Sign() != 0 {
			return false
		}
	}
	return true
}
