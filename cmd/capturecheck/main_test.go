package main

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

const testCaseJSON = `{
  "name": "test-case",
  "block": 100,
  "pre_block": 99,
  "onchain": {
    "tx": "0xabc",
    "start": "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1",
    "input": "1000",
    "net": "10",
    "gas_used": 5,
    "gas_price": "7",
    "path": [
      {
        "address": "0xc6962004f452bE9203591991D15f6b388e09E8D0",
        "kind": "v3",
        "token0": "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1",
        "token1": "0xaf88d065e77c8cC2239327C5EDb3A432268e5831",
        "fee": 500,
        "tick_spacing": 10
      },
      {
        "address": "0x09328be718d89228a985471bb470988a34d0b162",
        "kind": "slipstream",
        "token0": "0x004B506865409877C9fA29bfb1ebA929984B9bbC",
        "token1": "0xaf88d065e77c8cC2239327C5EDb3A432268e5831",
        "fee": 10,
        "tick_spacing": 1
      },
      {
        "address": "0x14cecb8a37013853ad071fc877705a8ecc9e77b7",
        "kind": "slipstream",
        "token0": "0x004B506865409877C9fA29bfb1ebA929984B9bbC",
        "token1": "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1",
        "fee": 586,
        "tick_spacing": 100
      }
    ],
    "legs": [
      {
        "pool": "0xc6962004f452bE9203591991D15f6b388e09E8D0",
        "token_in": "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1",
        "amount_in": "1000",
        "token_out": "0xaf88d065e77c8cC2239327C5EDb3A432268e5831",
        "amount_out": "900"
      },
      {
        "pool": "0x09328be718d89228a985471bb470988a34d0b162",
        "token_in": "0xaf88d065e77c8cC2239327C5EDb3A432268e5831",
        "amount_in": "900",
        "token_out": "0x004B506865409877C9fA29bfb1ebA929984B9bbC",
        "amount_out": "800"
      },
      {
        "pool": "0x14cecb8a37013853ad071fc877705a8ecc9e77b7",
        "token_in": "0x004B506865409877C9fA29bfb1ebA929984B9bbC",
        "amount_in": "800",
        "token_out": "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1",
        "amount_out": "1010"
      }
    ]
  }
}`

const (
	wethAddress = "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"
	usdcAddress = "0xaf88d065e77c8cc2239327c5edb3a432268e5831"
	altAddress  = "0x004b506865409877c9fa29bfb1eba929984b9bbc"
)

func TestDecodeCase(t *testing.T) {
	c, err := decodeCase([]byte(testCaseJSON))
	if err != nil {
		t.Fatalf("decodeCase: %v", err)
	}

	if c.Name != "test-case" {
		t.Errorf("name = %q, want test-case", c.Name)
	}
	if c.Block != 100 || c.PreBlock != 99 {
		t.Errorf("block/pre_block = %d/%d, want 100/99", c.Block, c.PreBlock)
	}
	if c.OnChain.Tx != "0xabc" {
		t.Errorf("tx = %q, want 0xabc", c.OnChain.Tx)
	}
	if got := c.OnChain.Start.Hex(); got != wethAddress {
		t.Errorf("start = %s, want %s", got, wethAddress)
	}
	if got := c.OnChain.Input.String(); got != "1000" {
		t.Errorf("input = %s, want 1000", got)
	}
	if got := c.OnChain.Net.String(); got != "10" {
		t.Errorf("net = %s, want 10", got)
	}
	if c.OnChain.GasUsed != 5 {
		t.Errorf("gas_used = %d, want 5", c.OnChain.GasUsed)
	}
	if got := c.OnChain.GasPrice.String(); got != "7" {
		t.Errorf("gas_price = %s, want 7", got)
	}

	if len(c.OnChain.Path) != 3 {
		t.Fatalf("path len = %d, want 3", len(c.OnChain.Path))
	}
	if c.OnChain.Path[0].Kind != domain.PoolKindV3 {
		t.Errorf("path[0].kind = %d, want V3", c.OnChain.Path[0].Kind)
	}
	if c.OnChain.Path[1].Kind != domain.PoolKindSlipstream {
		t.Errorf("path[1].kind = %d, want Slipstream", c.OnChain.Path[1].Kind)
	}
	if got := c.OnChain.Path[2].Fee; got != 586 {
		t.Errorf("path[2].fee = %d, want 586", got)
	}
	if got := c.OnChain.Path[2].TickSpacing; got != 100 {
		t.Errorf("path[2].tick_spacing = %d, want 100", got)
	}
	if got := c.OnChain.Path[0].Token1.Hex(); got != usdcAddress {
		t.Errorf("path[0].token1 = %s, want %s", got, usdcAddress)
	}

	if len(c.OnChain.Legs) != 3 {
		t.Fatalf("legs len = %d, want 3", len(c.OnChain.Legs))
	}
	if got := c.OnChain.Legs[0].AmountIn.String(); got != "1000" {
		t.Errorf("legs[0].amount_in = %s, want 1000", got)
	}
	if got := c.OnChain.Legs[2].AmountOut.String(); got != "1010" {
		t.Errorf("legs[2].amount_out = %s, want 1010", got)
	}
	if got := c.OnChain.Legs[1].TokenOut.Hex(); got != altAddress {
		t.Errorf("legs[1].token_out = %s, want %s", got, altAddress)
	}

	cycle := c.onChainCycle()
	if cycle.Len != 3 || len(cycle.Pools) != 3 {
		t.Errorf("cycle len = %d/%d, want 3/3", cycle.Len, len(cycle.Pools))
	}
	if cycle.Start != c.OnChain.Start {
		t.Errorf("cycle start = %s, want %s", cycle.Start, c.OnChain.Start)
	}
	if cycle.Pools[1].Kind != domain.PoolKindSlipstream {
		t.Errorf("cycle pool[1] kind = %d, want Slipstream", cycle.Pools[1].Kind)
	}
}

func TestDecodeCaseRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name: "unsupported kind",
			mutate: func(s string) string {
				return strings.Replace(s, `"kind": "v3"`, `"kind": "v2"`, 1)
			},
			wantErr: "unsupported pool kind",
		},
		{
			name: "pre block not block-1",
			mutate: func(s string) string {
				return strings.Replace(s, `"pre_block": 99`, `"pre_block": 98`, 1)
			},
			wantErr: "is not block-1",
		},
		{
			name: "malformed amount",
			mutate: func(s string) string {
				return strings.Replace(s, `"input": "1000"`, `"input": "10_00"`, 1)
			},
			wantErr: "not a base-10 integer",
		},
		{
			name: "leg does not swap through its pool",
			mutate: func(s string) string {
				return strings.Replace(s,
					`"token_out": "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1",`+"\n"+`        "amount_out": "1010"`,
					`"token_out": "0xaf88d065e77c8cC2239327C5EDb3A432268e5831",`+"\n"+`        "amount_out": "1010"`,
					1)
			},
			wantErr: "is not a swap through pool",
		},
		{
			name: "missing name",
			mutate: func(s string) string {
				return strings.Replace(s, `"name": "test-case"`, `"name": ""`, 1)
			},
			wantErr: "missing name",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeCase([]byte(test.mutate(testCaseJSON)))
			if err == nil {
				t.Fatal("decodeCase succeeded, want error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func testAddress(b byte) domain.Address {
	var address domain.Address
	address[19] = b
	return address
}

func testPool(b byte) domain.PoolKey {
	return domain.PoolKey{Address: testAddress(b), Kind: domain.PoolKindV3}
}

func TestDecodeCaseDefaultsUniverseToPath(t *testing.T) {
	c, err := decodeCase([]byte(testCaseJSON))
	if err != nil {
		t.Fatalf("decodeCase: %v", err)
	}
	if len(c.Universe) != len(c.OnChain.Path) {
		t.Fatalf("universe len = %d, want %d", len(c.Universe), len(c.OnChain.Path))
	}
	for i, spec := range c.OnChain.Path {
		if c.Universe[i] != spec {
			t.Errorf("universe[%d] = %+v, want path spec %+v", i, c.Universe[i], spec)
		}
	}

	c.Universe[0].Fee = 1
	if c.OnChain.Path[0].Fee == 1 {
		t.Error("universe aliases onchain.path; want a copy")
	}
}

func TestDecodeCaseAcceptsUniverseAlternative(t *testing.T) {
	base, err := decodeCase([]byte(testCaseJSON))
	if err != nil {
		t.Fatalf("decodeCase: %v", err)
	}
	alternative := poolSpec{
		Address:     testAddress(0x77),
		Kind:        domain.PoolKindV3,
		Token0:      base.OnChain.Path[0].Token0,
		Token1:      base.OnChain.Path[0].Token1,
		Fee:         3000,
		TickSpacing: 60,
	}
	universe := append(poolFiles(base.OnChain.Path), poolFile(alternative))

	c, err := decodeCase([]byte(testCaseWithUniverse(t, universe)))
	if err != nil {
		t.Fatalf("decodeCase: %v", err)
	}
	if len(c.Universe) != 4 {
		t.Fatalf("universe len = %d, want 4", len(c.Universe))
	}
	if got := c.Universe[3]; got != alternative {
		t.Errorf("universe[3] = %+v, want alternative %+v", got, alternative)
	}
	if len(c.OnChain.Path) != 3 {
		t.Errorf("path len = %d, want 3", len(c.OnChain.Path))
	}

	specs := seedSpecs(c)
	if len(specs) != 4 {
		t.Fatalf("seed specs = %d, want 4", len(specs))
	}
	seeded := specs[3]
	if seeded.Address != alternative.Address || seeded.Kind != domain.PoolKindV3 ||
		seeded.Fee != 3000 || seeded.TickSpacing != 60 {
		t.Errorf("seeded alternative = %+v, want address %s v3 fee 3000 tick spacing 60",
			seeded, alternative.Address)
	}
}

func TestDecodeUniverseRejectsInvalid(t *testing.T) {
	base, err := decodeCase([]byte(testCaseJSON))
	if err != nil {
		t.Fatalf("decodeCase: %v", err)
	}
	path := base.OnChain.Path

	mismatchedFee := append([]poolSpec(nil), path...)
	mismatchedFee[1].Fee = 11
	mismatchedTickSpacing := append([]poolSpec(nil), path...)
	mismatchedTickSpacing[2].TickSpacing = 200
	duplicate := append(append([]poolSpec(nil), path...), path[0])

	tests := []struct {
		name     string
		universe []poolSpec
		wantErr  []string
	}{
		{
			name:     "missing path pool",
			universe: path[:2],
			wantErr:  []string{"missing onchain.path[2] pool", path[2].Address.Hex()},
		},
		{
			name:     "path pool with different fee",
			universe: mismatchedFee,
			wantErr:  []string{"pool " + path[1].Address.Hex() + " does not match onchain.path[1]", "fee 11, want 10"},
		},
		{
			name:     "path pool with different tick spacing",
			universe: mismatchedTickSpacing,
			wantErr:  []string{"does not match onchain.path[2]", "tick_spacing 200, want 100"},
		},
		{
			name:     "duplicate pool",
			universe: duplicate,
			wantErr:  []string{"universe[3]: pool " + path[0].Address.Hex() + " repeats"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeUniverse(poolFiles(test.universe), path)
			if err == nil {
				t.Fatal("decodeUniverse succeeded, want error")
			}
			for _, want := range test.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// poolFile renders a validated spec back into the case JSON shape.
func poolFile(spec poolSpec) pathPoolFile {
	kind := "v3"
	if spec.Kind == domain.PoolKindSlipstream {
		kind = "slipstream"
	}
	return pathPoolFile{
		Address:     spec.Address.Hex(),
		Kind:        kind,
		Token0:      spec.Token0.Hex(),
		Token1:      spec.Token1.Hex(),
		Fee:         spec.Fee,
		TickSpacing: spec.TickSpacing,
	}
}

func poolFiles(specs []poolSpec) []pathPoolFile {
	files := make([]pathPoolFile, len(specs))
	for i, spec := range specs {
		files[i] = poolFile(spec)
	}
	return files
}

// testCaseWithUniverse injects a universe list into the base case JSON.
func testCaseWithUniverse(t *testing.T, universe []pathPoolFile) string {
	t.Helper()
	data, err := json.Marshal(universe)
	if err != nil {
		t.Fatalf("marshal universe: %v", err)
	}
	return strings.Replace(testCaseJSON,
		`"pre_block": 99,`,
		`"pre_block": 99, "universe": `+string(data)+`,`,
		1)
}

func TestSameCycle(t *testing.T) {
	tokenA := testAddress(0x11)
	tokenB := testAddress(0x22)
	tokenC := testAddress(0x33)
	pAB := testPool(0x01)
	pBC := testPool(0x02)
	pCA := testPool(0x03)
	pX := testPool(0x04)
	pY := testPool(0x05)

	pairs := map[domain.PoolKey][2]domain.Address{
		pAB: {tokenA, tokenB},
		pBC: {tokenB, tokenC},
		pCA: {tokenC, tokenA},
		pX:  {tokenA, tokenB},
		pY:  {tokenA, tokenB},
	}
	tokens := func(key domain.PoolKey) (domain.Address, domain.Address, bool) {
		pair, ok := pairs[key]
		return pair[0], pair[1], ok
	}
	cycle := func(start domain.Address, pools ...domain.PoolKey) graph.Cycle {
		return graph.Cycle{Pools: pools, Start: start, Len: len(pools)}
	}

	tests := []struct {
		name string
		a    graph.Cycle
		b    graph.Cycle
		want bool
	}{
		{
			name: "exact",
			a:    cycle(tokenA, pAB, pBC, pCA),
			b:    cycle(tokenA, pAB, pBC, pCA),
			want: true,
		},
		{
			name: "valid rotation",
			a:    cycle(tokenA, pAB, pBC, pCA),
			b:    cycle(tokenB, pBC, pCA, pAB),
			want: true,
		},
		{
			name: "rotated start mismatch",
			a:    cycle(tokenA, pAB, pBC, pCA),
			b:    cycle(tokenC, pBC, pCA, pAB),
			want: false,
		},
		{
			name: "reversed",
			a:    cycle(tokenA, pAB, pBC, pCA),
			b:    cycle(tokenA, pCA, pBC, pAB),
			want: false,
		},
		{
			name: "two pool reversed",
			a:    cycle(tokenA, pX, pY),
			b:    cycle(tokenA, pY, pX),
			want: false,
		},
		{
			name: "different pool set",
			a:    cycle(tokenA, pAB, pBC, pCA),
			b:    cycle(tokenA, pAB, pBC, pX),
			want: false,
		},
		{
			name: "length mismatch",
			a:    cycle(tokenA, pAB, pBC, pCA),
			b:    cycle(tokenA, pAB, pBC),
			want: false,
		},
		{
			name: "empty",
			a:    graph.Cycle{},
			b:    graph.Cycle{},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sameCycle(test.a, test.b, tokens); got != test.want {
				t.Fatalf("sameCycle = %t, want %t", got, test.want)
			}
			if got := sameCycle(test.b, test.a, tokens); got != test.want {
				t.Fatalf("sameCycle reversed arguments = %t, want %t", got, test.want)
			}
		})
	}
}

func TestNotionalByToken(t *testing.T) {
	c, err := decodeCase([]byte(testCaseJSON))
	if err != nil {
		t.Fatalf("decodeCase: %v", err)
	}
	notional := notionalByToken(c.OnChain.Legs)

	checks := []struct {
		token domain.Address
		want  string
	}{
		{c.OnChain.Path[0].Token0, "1000"},
		{c.OnChain.Path[1].Token1, "900"},
		{c.OnChain.Path[2].Token0, "800"},
	}
	for _, check := range checks {
		amount, ok := notional[check.token]
		if !ok {
			t.Fatalf("notional missing token %s", check.token)
		}
		if amount.String() != check.want {
			t.Errorf("notional[%s] = %s, want %s", check.token, amount, check.want)
		}
	}
	if got := notional[testAddress(0x99)]; got != nil {
		t.Errorf("notional for unknown token = %s, want nil", got)
	}
}

func TestDiffBps(t *testing.T) {
	tests := []struct {
		name string
		got  int64
		want int64
		bps  string
	}{
		{name: "higher", got: 101, want: 100, bps: "100.0000"},
		{name: "lower", got: 99, want: 100, bps: "-100.0000"},
		{name: "equal", got: 100, want: 100, bps: "0.0000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ratio := diffBps(big.NewInt(test.got), big.NewInt(test.want))
			if ratio == nil {
				t.Fatal("diffBps returned nil")
			}
			if got := ratio.FloatString(4); got != test.bps {
				t.Fatalf("diffBps = %s, want %s", got, test.bps)
			}
		})
	}

	if ratio := diffBps(big.NewInt(1), big.NewInt(0)); ratio != nil {
		t.Fatalf("diffBps with zero reference = %s, want nil", ratio)
	}
	if got := formatBps(nil); got != "na" {
		t.Fatalf("formatBps(nil) = %q, want na", got)
	}
}
