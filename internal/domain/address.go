package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Address is a 20-byte EVM address. The zero value is the zero address.
type Address [20]byte

// ParseAddress decodes a hex address with or without a 0x prefix. Input must
// contain exactly 40 hex characters; uppercase digits are accepted. Hex() is
// the canonical representation.
func ParseAddress(s string) (Address, error) {
	var addr Address
	raw := trimHexPrefix(s)
	if len(raw) != 2*len(addr) {
		return Address{}, fmt.Errorf("%w: want %d hex characters, got %d", ErrInvalidAddress, 2*len(addr), len(raw))
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return Address{}, fmt.Errorf("%w: %v", ErrInvalidAddress, err)
	}
	copy(addr[:], decoded)
	return addr, nil
}

// Hex returns the address as lowercase 0x-prefixed hex.
func (a Address) Hex() string {
	return "0x" + hex.EncodeToString(a[:])
}

// String implements fmt.Stringer with the same output as Hex.
func (a Address) String() string {
	return a.Hex()
}

func trimHexPrefix(s string) string {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return s[2:]
	}
	return s
}
