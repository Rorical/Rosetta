package domain

import (
	"encoding/hex"
	"fmt"
)

// Hash is a 32-byte value, used for block hashes and transaction hashes.
type Hash [32]byte

// ParseHash decodes a hex hash with or without a 0x prefix. Input must contain
// exactly 64 hex characters; uppercase digits are accepted.
func ParseHash(s string) (Hash, error) {
	var hash Hash
	raw := trimHexPrefix(s)
	if len(raw) != 2*len(hash) {
		return Hash{}, fmt.Errorf("%w: want %d hex characters, got %d", ErrInvalidHash, 2*len(hash), len(raw))
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return Hash{}, fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}
	copy(hash[:], decoded)
	return hash, nil
}

// Hex returns the hash as lowercase 0x-prefixed hex.
func (h Hash) Hex() string {
	return "0x" + hex.EncodeToString(h[:])
}
