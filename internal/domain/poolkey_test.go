package domain

import (
	"slices"
	"testing"
)

func TestPoolKeyZeroValue(t *testing.T) {
	var key PoolKey
	if key.Address != (Address{}) || key.Kind != 0 || key.PoolID != (Hash{}) || key.Hooks != (Address{}) {
		t.Fatalf("zero PoolKey = %+v, want every field zero", key)
	}
	if key.IsV4() {
		t.Fatal("zero PoolKey reports V4")
	}
	var other PoolKey
	if key != other {
		t.Fatal("two zero PoolKeys differ; the struct must stay comparable")
	}
}

func TestPoolKeyIsV4(t *testing.T) {
	tests := []struct {
		name string
		kind PoolKind
		want bool
	}{
		{"v2", PoolKindV2, false},
		{"v3", PoolKindV3, false},
		{"v4", PoolKindV4, true},
		{"camelot v3", PoolKindCamelotV3, false},
		{"slipstream", PoolKindSlipstream, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := PoolKey{Address: Address{0x01}, Kind: test.kind, PoolID: Hash{0x02}}
			if got := key.IsV4(); got != test.want {
				t.Fatalf("IsV4() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestComparePoolKeys(t *testing.T) {
	base := PoolKey{Address: Address{0x11}, Kind: PoolKindV4, PoolID: Hash{0x22}, Hooks: Address{0x33}}
	tests := []struct {
		name string
		a, b PoolKey
		want int
	}{
		{"equal", base, base, 0},
		{"address first", PoolKey{Address: Address{0x10}, Kind: PoolKindV4, PoolID: Hash{0xff}}, base, -1},
		{"address before kind", PoolKey{Address: Address{0x12}}, PoolKey{Address: Address{0x11}, Kind: PoolKindSlipstream}, 1},
		{"kind before pool id", PoolKey{Address: Address{0x11}, Kind: PoolKindV3, PoolID: Hash{0xff}}, PoolKey{Address: Address{0x11}, Kind: PoolKindV4}, -1},
		{"pool id before hooks", PoolKey{Address: Address{0x11}, Kind: PoolKindV4, PoolID: Hash{0x21}, Hooks: Address{0xff}}, PoolKey{Address: Address{0x11}, Kind: PoolKindV4, PoolID: Hash{0x22}}, -1},
		{"hooks differ", base, PoolKey{Address: Address{0x11}, Kind: PoolKindV4, PoolID: Hash{0x22}, Hooks: Address{0x34}}, -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ComparePoolKeys(test.a, test.b); got != test.want {
				t.Fatalf("ComparePoolKeys() = %d, want %d", got, test.want)
			}
			if got := ComparePoolKeys(test.b, test.a); got != -test.want {
				t.Fatalf("reverse ComparePoolKeys() = %d, want %d", got, -test.want)
			}
		})
	}
}

func TestComparePoolKeysOrdersV4Siblings(t *testing.T) {
	manager := Address{0x36}
	first := PoolKey{Address: manager, Kind: PoolKindV4, PoolID: Hash{0x01}}
	second := PoolKey{Address: manager, Kind: PoolKindV4, PoolID: Hash{0x02}}
	keys := []PoolKey{second, first}
	slices.SortFunc(keys, ComparePoolKeys)
	if keys[0] != first || keys[1] != second {
		t.Fatalf("sorted keys = %+v, want first then second", keys)
	}
}
