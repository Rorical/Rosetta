package bootstrap

import (
	"context"
	"errors"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestSeedRejectsInvalidSpecsBeforeMutation(t *testing.T) {
	addr := domain.Address{0xa6}
	other := domain.Address{0xa7}
	validV2 := v2Spec(addr)
	validV3 := v3Spec(addr)

	tests := []struct {
		name     string
		specs    []Spec
		want     error
		wantAlso error
	}{
		{
			name:  "duplicate address",
			specs: []Spec{validV2, v2Spec(addr)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "duplicate address across kinds",
			specs: []Spec{validV2, v3Spec(addr)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "unsupported kind",
			specs: []Spec{{Address: other, Token0: testToken0, Token1: testToken1, Kind: domain.PoolKind(99), Fee: 30}},
			want:  ErrUnsupportedKind,
		},
		{
			name:  "v4 wrong pool manager",
			specs: []Spec{withAddress(v4Spec(t, 500), other)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "v4 zero pool id",
			specs: []Spec{withPoolID(v4Spec(t, 500), domain.Hash{})},
			want:  ErrInvalidSpec,
		},
		{
			name:     "v4 hooked pool",
			specs:    []Spec{withHooks(v4Spec(t, 500), domain.Address{0xcc})},
			want:     ErrInvalidSpec,
			wantAlso: uniswapv4.ErrUnsupportedHooks,
		},
		{
			name:  "v4 zero token1",
			specs: []Spec{withTokens(v4Spec(t, 500), testToken0, domain.Address{})},
			want:  ErrInvalidSpec,
		},
		{
			name:  "v4 equal tokens",
			specs: []Spec{withTokens(v4Spec(t, 500), testToken0, testToken0)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "v4 reversed tokens",
			specs: []Spec{withTokens(v4Spec(t, 500), testToken1, testToken0)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "v4 without tick spacing",
			specs: []Spec{withTickSpacing(v4Spec(t, 500), 0)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "v4 fee above range",
			specs: []Spec{withFee(v4Spec(t, 500), maxFeePips)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "v4 and non-v4 sharing the manager address",
			specs: []Spec{v4Spec(t, 500), withAddress(validV3, uniswapv4.PoolManager())},
			want:  ErrInvalidSpec,
		},
		{
			name:  "zero pool address",
			specs: []Spec{{Token0: testToken0, Token1: testToken1, Kind: domain.PoolKindV2, Fee: 30}},
			want:  ErrInvalidSpec,
		},
		{
			name:  "zero token0",
			specs: []Spec{withTokens(validV2, domain.Address{}, testToken1)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "zero token1",
			specs: []Spec{withTokens(validV2, testToken0, domain.Address{})},
			want:  ErrInvalidSpec,
		},
		{
			name:  "equal tokens",
			specs: []Spec{withTokens(validV2, testToken0, testToken0)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "missing v2 fee",
			specs: []Spec{withFee(validV2, 0)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "v3 fee above range",
			specs: []Spec{withFee(validV3, maxFeePips)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "missing tick spacing",
			specs: []Spec{withTickSpacing(validV3, 0)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "non-v4 with pool id",
			specs: []Spec{withPoolID(validV3, domain.Hash{0x01})},
			want:  ErrInvalidSpec,
		},
		{
			name:  "camelot v3 without tick spacing",
			specs: []Spec{withTickSpacing(camelotV3Spec(addr), 0)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "slipstream without tick spacing",
			specs: []Spec{withTickSpacing(slipstreamSpec(addr), 0)},
			want:  ErrInvalidSpec,
		},
		{
			name:  "shallow non-v3",
			specs: []Spec{withShallow(v2Spec(addr))},
			want:  ErrInvalidSpec,
		},
		{
			name:  "shallow v4",
			specs: []Spec{withShallow(v4Spec(t, 500))},
			want:  ErrInvalidSpec,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeCaller()
			store := state.New()
			err := Seed(context.Background(), caller, store, test.specs, testBlockRef(1), 1)
			if !errors.Is(err, test.want) {
				t.Fatalf("Seed error = %v, want %v", err, test.want)
			}
			if test.wantAlso != nil && !errors.Is(err, test.wantAlso) {
				t.Fatalf("Seed error = %v, want also %v", err, test.wantAlso)
			}
			if len(caller.calls) != 0 {
				t.Fatalf("validation failure made %d contract calls", len(caller.calls))
			}
			if dirty := store.Dirty(); len(dirty) != 0 {
				t.Fatalf("validation failure marked pools dirty: %v", dirty)
			}
			for _, spec := range test.specs {
				if _, ok := store.Pool(spec.key()); ok {
					t.Fatalf("validation failure stored pool %s", spec.Address)
				}
			}
		})
	}
}

// TestValidateSeedAcceptsV4Siblings pins what a V4 spec must and must not
// carry: two pool IDs behind one PoolManager are distinct entries, and the
// configured LP fee is optional because slot0 replaces it.
func TestValidateSeedAcceptsV4Siblings(t *testing.T) {
	first := v4Spec(t, 500)
	second := withV4PoolID(t, withFee(v4Spec(t, 3000), 0))

	if err := validateSeed(newFakeCaller(), state.New(), []Spec{first, second}, testBlockRef(1)); err != nil {
		t.Fatalf("validateSeed: %v", err)
	}
}

// TestValidateV4PoolID checks that the configured pool ID must commit to the
// rest of the key. Reading with a mismatched spacing would load the right pool
// and decode its bitmap at the wrong spacing, so the seed is rejected before
// any StateView call.
func TestValidateV4PoolID(t *testing.T) {
	valid := v4Spec(t, 500)
	if err := validateV4Spec(valid); err != nil {
		t.Fatalf("validateV4Spec(valid): %v", err)
	}

	mismatched := withTickSpacing(valid, 60)
	caller := newFakeCaller()
	store := state.New()
	err := Seed(context.Background(), caller, store, []Spec{mismatched}, testBlockRef(1), 1)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Seed error = %v, want ErrInvalidSpec", err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("pool ID mismatch made %d contract calls", len(caller.calls))
	}
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("pool ID mismatch marked pools dirty: %v", dirty)
	}
}

func TestSeedRejectsUnusableArguments(t *testing.T) {
	specs := []Spec{v2Spec(domain.Address{0xa8})}
	tests := []struct {
		name   string
		caller pool.ContractCaller
		store  *state.Store
		specs  []Spec
		ref    domain.BlockRef
	}{
		{name: "nil caller", caller: nil, store: state.New(), specs: specs, ref: testBlockRef(1)},
		{name: "nil store", caller: newFakeCaller(), store: nil, specs: specs, ref: testBlockRef(1)},
		{name: "nil specs", caller: newFakeCaller(), store: state.New(), specs: nil, ref: testBlockRef(1)},
		{name: "empty specs", caller: newFakeCaller(), store: state.New(), specs: []Spec{}, ref: testBlockRef(1)},
		{name: "zero block number", caller: newFakeCaller(), store: state.New(), specs: specs, ref: domain.BlockRef{Hash: domain.Hash{0x01}}},
		{name: "zero block hash", caller: newFakeCaller(), store: state.New(), specs: specs, ref: domain.BlockRef{Number: 1}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Seed(context.Background(), test.caller, test.store, test.specs, test.ref, 1)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("Seed error = %v, want %v", err, ErrInvalidSpec)
			}
			if caller, ok := test.caller.(*fakeCaller); ok && len(caller.calls) != 0 {
				t.Fatalf("invalid arguments made %d contract calls", len(caller.calls))
			}
			if test.store != nil {
				if dirty := test.store.Dirty(); len(dirty) != 0 {
					t.Fatalf("invalid arguments marked pools dirty: %v", dirty)
				}
			}
		})
	}
}
