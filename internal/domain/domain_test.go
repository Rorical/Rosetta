package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAddressRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"zero with prefix", "0x0000000000000000000000000000000000000000", "0x0000000000000000000000000000000000000000"},
		{"uppercase", "0x52908400098527886E0F7030069857D2E4169EE7", "0x52908400098527886e0f7030069857d2e4169ee7"},
		{"without prefix", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		{"uppercase prefix", "0XDEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF", "0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, err := ParseAddress(test.in)
			if err != nil {
				t.Fatalf("ParseAddress(%q): %v", test.in, err)
			}
			if got := address.Hex(); got != test.want {
				t.Fatalf("Hex() = %q, want %q", got, test.want)
			}
			if got := address.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
			roundTrip, err := ParseAddress(address.Hex())
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			if roundTrip != address {
				t.Fatalf("round trip mismatch: %v != %v", roundTrip, address)
			}
		})
	}
}

func TestParseAddressInvalid(t *testing.T) {
	valid := strings.Repeat("ab", 20)
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"prefix only", "0x"},
		{"short", "0x" + valid[:len(valid)-2]},
		{"long", "0x" + valid + "ab"},
		{"non hex", "0x" + strings.Repeat("zz", 20)},
		{"space", "0x" + valid[:len(valid)-1] + " "},
		{"odd length", "0x" + valid[:len(valid)-1]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseAddress(test.in)
			if !errors.Is(err, ErrInvalidAddress) {
				t.Fatalf("ParseAddress(%q) error = %v, want ErrInvalidAddress", test.in, err)
			}
		})
	}
}

func TestParseHashRoundTrip(t *testing.T) {
	raw := strings.Repeat("0123456789abcdef", 4)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"with prefix", "0x" + raw, "0x" + raw},
		{"without prefix", raw, "0x" + raw},
		{"uppercase", "0X" + strings.ToUpper(raw), "0x" + raw},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hash, err := ParseHash(test.in)
			if err != nil {
				t.Fatalf("ParseHash(%q): %v", test.in, err)
			}
			if got := hash.Hex(); got != test.want {
				t.Fatalf("Hex() = %q, want %q", got, test.want)
			}
			roundTrip, err := ParseHash(hash.Hex())
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			if roundTrip != hash {
				t.Fatalf("round trip mismatch")
			}
		})
	}
}

func TestParseHashInvalid(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"short", "0x" + valid[:len(valid)-2]},
		{"long", "0x" + valid + "ab"},
		{"non hex", "0x" + strings.Repeat("gg", 32)},
		{"odd length", "0x" + valid[:len(valid)-1]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseHash(test.in)
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("ParseHash(%q) error = %v, want ErrInvalidHash", test.in, err)
			}
		})
	}
}

func TestDomainConstants(t *testing.T) {
	if ArbitrumOneChainID != 42161 {
		t.Fatalf("ArbitrumOneChainID = %d, want 42161", ArbitrumOneChainID)
	}
	kinds := map[PoolKind]bool{
		PoolKindV2:         true,
		PoolKindV3:         true,
		PoolKindV4:         true,
		PoolKindCamelotV3:  true,
		PoolKindSlipstream: true,
	}
	if len(kinds) != 5 {
		t.Fatalf("pool kinds are not distinct: %v", kinds)
	}
	// Published kinds keep their wire values so snapshots and persisted
	// state stay readable across the additions.
	wantValues := map[PoolKind]uint8{
		PoolKindV2:         1,
		PoolKindV3:         2,
		PoolKindV4:         3,
		PoolKindCamelotV3:  4,
		PoolKindSlipstream: 5,
	}
	for kind, want := range wantValues {
		if uint8(kind) != want {
			t.Fatalf("pool kind value = %d, want %d", kind, want)
		}
	}
}
