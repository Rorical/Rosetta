package uniswapv4

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
)

// PoolID returns the Uniswap V4 pool ID of a static pool key:
// keccak256(abi.encode(currency0, currency1, fee, tickSpacing, hooks)).
//
// Each field occupies one 32-byte ABI word. Addresses are left-padded, fee is
// uint24 widened to a uint256 word, and tickSpacing is int24 widened with sign
// extension. Negative spacing is rejected because the rest of this package
// only models non-negative spacing; every on-chain pool satisfies that.
//
// The ID commits to the fee and tick spacing, so comparing it with the
// configured key catches a typo that would otherwise decode the tick bitmap at
// the wrong spacing.
func PoolID(token0, token1 domain.Address, fee uint32, tickSpacing int32, hooks domain.Address) (domain.Hash, error) {
	if tickSpacing < 0 {
		return domain.Hash{}, fmt.Errorf("%w: tick spacing %d must not be negative", ErrInvalidSpec, tickSpacing)
	}

	var encoded [5 * abiWordSize]byte
	copy(encoded[abiWordSize-20:], token0[:])
	copy(encoded[2*abiWordSize-20:], token1[:])
	binary.BigEndian.PutUint32(encoded[3*abiWordSize-4:], fee)
	binary.BigEndian.PutUint32(encoded[4*abiWordSize-4:], uint32(tickSpacing))
	copy(encoded[5*abiWordSize-20:], hooks[:])

	hasher := sha3.NewLegacyKeccak256()
	// hash.Hash.Write never returns an error for this digest.
	hasher.Write(encoded[:])
	var id domain.Hash
	copy(id[:], hasher.Sum(nil))
	return id, nil
}
