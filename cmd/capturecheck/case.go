package main

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// caseFile mirrors the case JSON. Amounts stay strings until decodeCase
// validates them, so a bad value fails with the field name instead of a
// zero-value amount.
type caseFile struct {
	Name     string         `json:"name"`
	Block    uint64         `json:"block"`
	PreBlock uint64         `json:"pre_block"`
	Universe []pathPoolFile `json:"universe"`
	OnChain  onChainFile    `json:"onchain"`
}

type onChainFile struct {
	Tx       string         `json:"tx"`
	Start    string         `json:"start"`
	Input    string         `json:"input"`
	Net      string         `json:"net"`
	GasUsed  uint64         `json:"gas_used"`
	GasPrice string         `json:"gas_price"`
	Path     []pathPoolFile `json:"path"`
	Legs     []legFile      `json:"legs"`
}

type pathPoolFile struct {
	Address     string `json:"address"`
	Kind        string `json:"kind"`
	Token0      string `json:"token0"`
	Token1      string `json:"token1"`
	Fee         uint32 `json:"fee"`
	TickSpacing int32  `json:"tick_spacing"`
}

type legFile struct {
	Pool      string `json:"pool"`
	TokenIn   string `json:"token_in"`
	AmountIn  string `json:"amount_in"`
	TokenOut  string `json:"token_out"`
	AmountOut string `json:"amount_out"`
}

// caseData is the validated case.
type caseData struct {
	Name     string
	Block    uint64
	PreBlock uint64
	// Universe is every pool the engine sees: the on-chain path plus optional
	// alternatives to rank against. It defaults to the path.
	Universe []poolSpec
	OnChain  onChainData
}

// onChainData is the executed arbitrage: the pools in traversal order, the leg
// amounts, the input, and the realized net.
type onChainData struct {
	Tx       string
	Start    domain.Address
	Input    *big.Int
	Net      *big.Int
	GasUsed  uint64
	GasPrice *big.Int
	Path     []poolSpec
	Legs     []swapLeg
}

// poolSpec identifies one seeded pool.
type poolSpec struct {
	Address     domain.Address
	Kind        domain.PoolKind
	Token0      domain.Address
	Token1      domain.Address
	Fee         uint32
	TickSpacing int32
}

func (p poolSpec) key() domain.PoolKey {
	return domain.PoolKey{Address: p.Address, Kind: p.Kind}
}

// swapLeg is one executed swap. AmountIn and AmountOut are the gross token
// amounts entering and leaving the pool.
type swapLeg struct {
	Pool      domain.Address
	TokenIn   domain.Address
	AmountIn  *big.Int
	TokenOut  domain.Address
	AmountOut *big.Int
}

// decodeCase parses and validates one case file.
func decodeCase(data []byte) (caseData, error) {
	var file caseFile
	if err := json.Unmarshal(data, &file); err != nil {
		return caseData{}, fmt.Errorf("case: decode JSON: %w", err)
	}
	c := caseData{Name: file.Name, Block: file.Block, PreBlock: file.PreBlock}
	if err := validateHeader(c); err != nil {
		return caseData{}, err
	}
	onchain, err := decodeOnChain(file.OnChain)
	if err != nil {
		return caseData{}, err
	}
	c.OnChain = onchain
	universe, err := decodeUniverse(file.Universe, onchain.Path)
	if err != nil {
		return caseData{}, err
	}
	c.Universe = universe
	return c, nil
}

func validateHeader(c caseData) error {
	if c.Name == "" {
		return fmt.Errorf("case: missing name")
	}
	if c.Block == 0 {
		return fmt.Errorf("case: missing block")
	}
	if c.PreBlock == 0 {
		return fmt.Errorf("case: missing pre_block")
	}
	if c.PreBlock >= c.Block || c.Block-c.PreBlock != 1 {
		return fmt.Errorf("case: pre_block %d is not block-1 (%d)", c.PreBlock, c.Block)
	}
	return nil
}

func decodeOnChain(file onChainFile) (onChainData, error) {
	var onchain onChainData
	var err error

	onchain.Tx = file.Tx
	if onchain.Start, err = parseAddress("onchain.start", file.Start); err != nil {
		return onChainData{}, err
	}
	if onchain.Input, err = parsePositiveAmount("onchain.input", file.Input); err != nil {
		return onChainData{}, err
	}
	if onchain.Net, err = parseAmount("onchain.net", file.Net); err != nil {
		return onChainData{}, err
	}
	onchain.GasUsed = file.GasUsed
	if onchain.GasPrice, err = parseAmount("onchain.gas_price", file.GasPrice); err != nil {
		return onChainData{}, err
	}
	if onchain.GasPrice.Sign() < 0 {
		return onChainData{}, fmt.Errorf("case: onchain.gas_price %q: want non-negative", file.GasPrice)
	}

	if len(file.Path) == 0 {
		return onChainData{}, fmt.Errorf("case: onchain.path is empty")
	}
	if len(file.Legs) != len(file.Path) {
		return onChainData{}, fmt.Errorf("case: onchain.legs has %d entries for %d path pools",
			len(file.Legs), len(file.Path))
	}

	seenPools := make(map[domain.Address]struct{}, len(file.Path))
	onchain.Path = make([]poolSpec, len(file.Path))
	for i := range file.Path {
		spec, err := decodePoolSpec(fmt.Sprintf("onchain.path[%d]", i), file.Path[i])
		if err != nil {
			return onChainData{}, err
		}
		if _, exists := seenPools[spec.Address]; exists {
			return onChainData{}, fmt.Errorf("case: onchain.path[%d]: pool %s repeats", i, spec.Address)
		}
		seenPools[spec.Address] = struct{}{}
		onchain.Path[i] = spec
	}

	onchain.Legs = make([]swapLeg, len(file.Legs))
	for i := range file.Legs {
		leg, err := decodeLeg(i, file.Legs[i], onchain.Path[i])
		if err != nil {
			return onChainData{}, err
		}
		onchain.Legs[i] = leg
	}
	if err := validateLegChain(onchain); err != nil {
		return onChainData{}, err
	}
	return onchain, nil
}

// decodeUniverse validates the pool set the engine seeds, replays, and builds
// its cycle table from. An absent universe defaults to a copy of the on-chain
// path. An explicit universe may add alternatives, but every path pool must
// appear in it unchanged so the direct quote and rank comparison keep using
// the executed pools.
func decodeUniverse(files []pathPoolFile, path []poolSpec) ([]poolSpec, error) {
	if len(files) == 0 {
		universe := make([]poolSpec, len(path))
		copy(universe, path)
		return universe, nil
	}

	universe := make([]poolSpec, len(files))
	seen := make(map[domain.Address]struct{}, len(files))
	for i := range files {
		spec, err := decodePoolSpec(fmt.Sprintf("universe[%d]", i), files[i])
		if err != nil {
			return nil, err
		}
		if _, exists := seen[spec.Address]; exists {
			return nil, fmt.Errorf("case: universe[%d]: pool %s repeats", i, spec.Address)
		}
		seen[spec.Address] = struct{}{}
		universe[i] = spec
	}

	for i, spec := range path {
		if universeContains(universe, spec) {
			continue
		}
		if other, ok := universePool(universe, spec.Address); ok {
			return nil, fmt.Errorf("case: universe pool %s does not match onchain.path[%d]: %s",
				spec.Address, i, poolSpecDiff(other, spec))
		}
		return nil, fmt.Errorf("case: universe is missing onchain.path[%d] pool %s", i, spec.Address)
	}
	return universe, nil
}

func universeContains(universe []poolSpec, spec poolSpec) bool {
	for _, candidate := range universe {
		if candidate == spec {
			return true
		}
	}
	return false
}

// universePool finds the universe entry for an address. Only V3 and Slipstream
// pools are accepted, so an address identifies at most one pool.
func universePool(universe []poolSpec, address domain.Address) (poolSpec, bool) {
	for _, candidate := range universe {
		if candidate.Address == address {
			return candidate, true
		}
	}
	return poolSpec{}, false
}

// poolSpecDiff lists the fields where got differs from want. The caller only
// reaches it after universeContains failed for the same address.
func poolSpecDiff(got, want poolSpec) string {
	var diffs []string
	if got.Kind != want.Kind {
		diffs = append(diffs, fmt.Sprintf("kind %s, want %s", poolKindName(got.Kind), poolKindName(want.Kind)))
	}
	if got.Token0 != want.Token0 {
		diffs = append(diffs, fmt.Sprintf("token0 %s, want %s", got.Token0, want.Token0))
	}
	if got.Token1 != want.Token1 {
		diffs = append(diffs, fmt.Sprintf("token1 %s, want %s", got.Token1, want.Token1))
	}
	if got.Fee != want.Fee {
		diffs = append(diffs, fmt.Sprintf("fee %d, want %d", got.Fee, want.Fee))
	}
	if got.TickSpacing != want.TickSpacing {
		diffs = append(diffs, fmt.Sprintf("tick_spacing %d, want %d", got.TickSpacing, want.TickSpacing))
	}
	return strings.Join(diffs, ", ")
}

func decodePoolSpec(field string, file pathPoolFile) (poolSpec, error) {
	var spec poolSpec
	var err error

	if spec.Address, err = parseAddress(field+".address", file.Address); err != nil {
		return poolSpec{}, err
	}
	if spec.Kind, err = parsePoolKind(file.Kind); err != nil {
		return poolSpec{}, fmt.Errorf("case: %s.kind: %w", field, err)
	}
	if spec.Token0, err = parseAddress(field+".token0", file.Token0); err != nil {
		return poolSpec{}, err
	}
	if spec.Token1, err = parseAddress(field+".token1", file.Token1); err != nil {
		return poolSpec{}, err
	}
	if spec.Token0 == spec.Token1 {
		return poolSpec{}, fmt.Errorf("case: %s: token0 and token1 are equal", field)
	}
	if file.Fee == 0 {
		return poolSpec{}, fmt.Errorf("case: %s.fee: missing", field)
	}
	if file.TickSpacing <= 0 {
		return poolSpec{}, fmt.Errorf("case: %s.tick_spacing %d: want positive", field, file.TickSpacing)
	}
	spec.Fee = file.Fee
	spec.TickSpacing = file.TickSpacing
	return spec, nil
}

func decodeLeg(index int, file legFile, spec poolSpec) (swapLeg, error) {
	field := fmt.Sprintf("onchain.legs[%d]", index)
	var leg swapLeg
	var err error

	if leg.Pool, err = parseAddress(field+".pool", file.Pool); err != nil {
		return swapLeg{}, err
	}
	if leg.TokenIn, err = parseAddress(field+".token_in", file.TokenIn); err != nil {
		return swapLeg{}, err
	}
	if leg.TokenOut, err = parseAddress(field+".token_out", file.TokenOut); err != nil {
		return swapLeg{}, err
	}
	if leg.AmountIn, err = parsePositiveAmount(field+".amount_in", file.AmountIn); err != nil {
		return swapLeg{}, err
	}
	if leg.AmountOut, err = parsePositiveAmount(field+".amount_out", file.AmountOut); err != nil {
		return swapLeg{}, err
	}

	if leg.Pool != spec.Address {
		return swapLeg{}, fmt.Errorf("case: %s.pool %s does not match path pool %s",
			field, leg.Pool, spec.Address)
	}
	switch leg.TokenIn {
	case spec.Token0:
		if leg.TokenOut != spec.Token1 {
			return swapLeg{}, fmt.Errorf("case: %s: %s -> %s is not a swap through pool %s",
				field, leg.TokenIn, leg.TokenOut, spec.Address)
		}
	case spec.Token1:
		if leg.TokenOut != spec.Token0 {
			return swapLeg{}, fmt.Errorf("case: %s: %s -> %s is not a swap through pool %s",
				field, leg.TokenIn, leg.TokenOut, spec.Address)
		}
	default:
		return swapLeg{}, fmt.Errorf("case: %s.token_in %s is not a token of pool %s",
			field, leg.TokenIn, spec.Address)
	}
	return leg, nil
}

// validateLegChain checks that the legs form the token-simple cycle the graph
// comparison assumes: they start at the on-chain input token and return to it
// without revisiting a token.
func validateLegChain(onchain onChainData) error {
	first := onchain.Legs[0]
	if first.TokenIn != onchain.Start {
		return fmt.Errorf("case: onchain.legs[0].token_in %s does not match start %s",
			first.TokenIn, onchain.Start)
	}
	if first.AmountIn.Cmp(onchain.Input) != 0 {
		return fmt.Errorf("case: onchain.input %s does not match legs[0].amount_in %s",
			onchain.Input, first.AmountIn)
	}

	seen := map[domain.Address]struct{}{onchain.Start: {}}
	for i, leg := range onchain.Legs {
		if i > 0 && leg.TokenIn != onchain.Legs[i-1].TokenOut {
			return fmt.Errorf("case: onchain.legs[%d].token_in %s does not continue from legs[%d].token_out %s",
				i, leg.TokenIn, i-1, onchain.Legs[i-1].TokenOut)
		}
		if i == len(onchain.Legs)-1 {
			if leg.TokenOut != onchain.Start {
				return fmt.Errorf("case: onchain.legs[%d].token_out %s does not close the cycle at %s",
					i, leg.TokenOut, onchain.Start)
			}
			continue
		}
		if _, exists := seen[leg.TokenOut]; exists {
			return fmt.Errorf("case: onchain.legs[%d].token_out %s repeats a token; the path must be token-simple",
				i, leg.TokenOut)
		}
		seen[leg.TokenOut] = struct{}{}
	}
	return nil
}

// onChainCycle returns the executed path as a graph cycle in its own order and
// start token.
func (c caseData) onChainCycle() graph.Cycle {
	pools := make([]domain.PoolKey, len(c.OnChain.Path))
	for i, spec := range c.OnChain.Path {
		pools[i] = spec.key()
	}
	return graph.Cycle{Pools: pools, Start: c.OnChain.Start, Len: len(pools)}
}

// notionalByToken maps each leg's input token to its input amount. A cycle
// rotated to a token is then quoted at the size the on-chain trade used when it
// reached that token, so every rotation carries the same economic size.
func notionalByToken(legs []swapLeg) map[domain.Address]*big.Int {
	notional := make(map[domain.Address]*big.Int, len(legs))
	for _, leg := range legs {
		notional[leg.TokenIn] = new(big.Int).Set(leg.AmountIn)
	}
	return notional
}

func parsePoolKind(value string) (domain.PoolKind, error) {
	switch value {
	case "v3":
		return domain.PoolKindV3, nil
	case "slipstream":
		return domain.PoolKindSlipstream, nil
	default:
		return 0, fmt.Errorf("unsupported pool kind %q (supported: v3, slipstream)", value)
	}
}

func parseAddress(field, value string) (domain.Address, error) {
	address, err := domain.ParseAddress(value)
	if err != nil {
		return domain.Address{}, fmt.Errorf("case: %s %q: %w", field, value, err)
	}
	return address, nil
}

func parseAmount(field, value string) (*big.Int, error) {
	amount, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return nil, fmt.Errorf("case: %s %q: not a base-10 integer", field, value)
	}
	return amount, nil
}

func parsePositiveAmount(field, value string) (*big.Int, error) {
	amount, err := parseAmount(field, value)
	if err != nil {
		return nil, err
	}
	if amount.Sign() <= 0 {
		return nil, fmt.Errorf("case: %s %q: want positive", field, value)
	}
	return amount, nil
}
