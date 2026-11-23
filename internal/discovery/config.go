package discovery

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
)

// Defaults applied by LoadConfig to fields the document does not set.
const (
	defaultLookbackBlocks  uint64 = 50_000
	defaultRefreshBlocks   uint64 = 300
	defaultScanChunkBlocks uint64 = 10_000
	defaultMaxPools               = 32
	defaultMaxFee          uint32 = 10_000

	// maxFeePips is 100% in hundredths of a basis point, the largest fee the
	// V3 and V4 engines can represent.
	maxFeePips uint32 = 1_000_000
)

// ReplayBlockLimit bounds canonical receipt history while a background pool
// initialization performs provider reads. A separate byte cap also applies.
const ReplayBlockLimit = 1024

// Config controls discovery. LoadConfig fills the defaults; a Config built by
// hand must set the required fields itself, because New validates instead of
// filling them.
type Config struct {
	AdditionalEmitters []Emitter

	InventoryPath string
	Selection     *SelectionPolicy

	Deployments *domain.Deployments
	// LookbackBlocks is how many blocks before the head a refresh scans.
	LookbackBlocks uint64
	// RefreshBlocks is how often discovery ought to run. Discovery does not
	// schedule itself; the field is carried for the caller.
	RefreshBlocks uint64
	// ScanChunkBlocks is the widest block range sent to one eth_getLogs.
	ScanChunkBlocks uint64
	// HistoryChunkBlocks splits full-history lookups, such as a V4 pool's
	// Initialize, into ranges of this many blocks for nodes that time out on
	// the whole chain. Zero sends one full-history query.
	HistoryChunkBlocks uint64
	// HistoryChunksPerRound bounds the chunks one round queries, so a round
	// ends before the node prunes its anchor's state; the lookup resumes next
	// round. Zero means no bound.
	HistoryChunksPerRound int
	// MaxPools caps the discovered pools. Seeds do not count against it.
	MaxPools int
	// MinLiquidity drops pools whose active liquidity is below it. Pools with
	// zero active liquidity are dropped regardless, because they cannot fill a
	// quote. Nil means no floor, same as zero.
	MinLiquidity *big.Int
	// MaxFee drops pools whose fee exceeds it, in hundredths of a basis point.
	// Zero is valid: it keeps only zero-fee pools. Camelot must satisfy the
	// bound in both directions.
	MaxFee uint32
	// QuoteTokens restricts a pool's two tokens to this set. Nil or empty means
	// no restriction: every token pair is eligible.
	QuoteTokens []domain.Address
	// DenyTokenCode drops a pool when either token's deployed code pushes one
	// of these 4-byte selectors, usually a custom error. It names a token
	// template rather than one deployment: on Robinhood, several tokens from
	// one 24,417-byte template revert 0x0174639d(recipient) on any buy from
	// their pool to a recipient outside their allowlist.
	DenyTokenCode [][4]byte
	// Sources is the log streams to scan, in scan order.
	Sources []Source
	// Seeds are pools always included regardless of filters and ranking.
	Seeds []Spec
}

// LoadConfig decodes a JSON configuration document and fills defaults:
// lookback_blocks 50,000, refresh_blocks 300, scan_chunk_blocks 10,000,
// max_pools 32, max_fee 10,000, min_liquidity 0, sources all six. Unknown
// fields are rejected so a misspelled key cannot silently fall back to a
// default, and an explicit empty sources list is an error rather than "scan
// nothing". max_fee is inclusive; zero is a valid bound and keeps only
// zero-fee pools.
//
// quote_tokens is optional. A non-empty list keeps only pools whose two tokens
// both belong to it. An absent or empty list is global discovery: every token
// pair is eligible, which is higher risk, though max_pools, max_fee, and
// min_liquidity still bound the result and zero-liquidity pools are always
// dropped.
//
// deny_token_code_selectors is optional: 4-byte selectors, each matched as a
// PUSH4 in a token's deployed code. A pool with a matching token is rejected.
//
// seed_pools entries take address, token0, token1, kind, pool_id, hooks, fee,
// and tick_spacing. kind is "v2", "v3", "camelot_v3", "slipstream", or "v4".
// V2 seeds require an explicit fee in pips. pool_id and hooks are
// optional elsewhere. A V4 seed must name the PoolManager, have zero hooks, and
// carry a pool ID that matches its fee and tick spacing; its fee may be zero,
// and the ID commits to that zero. The bootstrap and quote paths reject hooked
// pools.
func LoadConfig(data []byte) (Config, error) { return LoadConfigForNetwork(data, nil) }

// LoadConfigForNetwork uses only the supplied chain deployments. Nil retains
// the Arbitrum One defaults for standalone callers.
func LoadConfigForNetwork(data []byte, deployments *domain.Deployments) (Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw rawConfig
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("%w: decode: %v", ErrInvalidConfig, err)
	}
	if err := decoder.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("%w: trailing data after the config object", ErrInvalidConfig)
	}

	config := Config{Deployments: deployments, InventoryPath: raw.InventoryPath, Selection: raw.Selection,
		LookbackBlocks:  defaultLookbackBlocks,
		RefreshBlocks:   defaultRefreshBlocks,
		ScanChunkBlocks: defaultScanChunkBlocks,
		MaxPools:        defaultMaxPools,
		MinLiquidity:    new(big.Int),
		MaxFee:          defaultMaxFee,
	}
	if raw.LookbackBlocks != nil {
		config.LookbackBlocks = *raw.LookbackBlocks
	}
	if raw.RefreshBlocks != nil {
		config.RefreshBlocks = *raw.RefreshBlocks
	}
	if raw.ScanChunkBlocks != nil {
		config.ScanChunkBlocks = *raw.ScanChunkBlocks
	}
	if raw.HistoryChunkBlocks != nil {
		config.HistoryChunkBlocks = *raw.HistoryChunkBlocks
	}
	if raw.HistoryChunksPerRound != nil {
		if *raw.HistoryChunksPerRound < 0 {
			return Config{}, fmt.Errorf("%w: history_chunks_per_round must not be negative", ErrInvalidConfig)
		}
		config.HistoryChunksPerRound = *raw.HistoryChunksPerRound
	}
	if raw.MaxPools != nil {
		config.MaxPools = *raw.MaxPools
	}
	if raw.MaxFee != nil {
		config.MaxFee = *raw.MaxFee
	}
	if raw.MinLiquidity != nil && *raw.MinLiquidity != "" {
		value, ok := new(big.Int).SetString(*raw.MinLiquidity, 10)
		if !ok {
			return Config{}, fmt.Errorf("%w: min_liquidity %q is not a decimal integer",
				ErrInvalidConfig, *raw.MinLiquidity)
		}
		config.MinLiquidity = value
	}

	for i, text := range raw.QuoteTokens {
		address, err := domain.ParseAddress(text)
		if err != nil {
			return Config{}, fmt.Errorf("%w: quote_tokens[%d]: %v", ErrInvalidConfig, i, err)
		}
		config.QuoteTokens = append(config.QuoteTokens, address)
	}
	for i, text := range raw.DenyTokenCode {
		raw, err := hex.DecodeString(strings.TrimPrefix(text, "0x"))
		if err != nil || len(raw) != 4 {
			return Config{}, fmt.Errorf("%w: deny_token_code_selectors[%d]: want 4 hex bytes", ErrInvalidConfig, i)
		}
		config.DenyTokenCode = append(config.DenyTokenCode, [4]byte(raw))
	}

	for _, entry := range raw.Emitters {
		source, err := ParseSource(entry.Source)
		if err != nil {
			return Config{}, err
		}
		address, err := domain.ParseAddress(entry.Address)
		if err != nil {
			return Config{}, err
		}
		config.AdditionalEmitters = append(config.AdditionalEmitters, Emitter{source, address})
	}
	config.Sources = allSources()
	if deployments != nil {
		config.Sources = nil
		for _, source := range append(allSources(), SourceVelodromeV2) {
			if source.Contract(deployments) != (domain.Address{}) {
				config.Sources = append(config.Sources, source)
			}
		}
	}
	if raw.Sources != nil {
		config.Sources = make([]Source, len(raw.Sources))
		for i, text := range raw.Sources {
			source, err := ParseSource(text)
			if err != nil {
				return Config{}, fmt.Errorf("%w: sources[%d]: %w", ErrInvalidConfig, i, err)
			}
			config.Sources[i] = source
		}
	}

	config.Seeds = make([]Spec, len(raw.SeedPools))
	for i, seed := range raw.SeedPools {
		spec, err := seed.spec()
		if err != nil {
			return Config{}, fmt.Errorf("%w: seed_pools[%d]: %w", ErrInvalidConfig, i, err)
		}
		config.Seeds[i] = spec
	}

	if err := config.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// validate checks the values New and LoadConfig both depend on. A zero
// LookbackBlocks is allowed: Discover takes an explicit range. A zero
// RefreshBlocks is allowed too: discovery does not schedule itself.
func (c Config) validate() error {
	if len(c.AdditionalEmitters) > 16 {
		return fmt.Errorf("too many discovery emitters")
	}
	seenEmitters := map[Emitter]bool{}
	for _, emitter := range c.Emitters() {
		if !emitter.Source.Valid() || emitter.Address == (domain.Address{}) || seenEmitters[emitter] {
			return fmt.Errorf("invalid or duplicate discovery emitter")
		}
		seenEmitters[emitter] = true
	}

	if c.Selection != nil {
		if err := c.Selection.Validate(); err != nil {
			return err
		}
	}
	if c.ScanChunkBlocks == 0 {
		return fmt.Errorf("%w: scan_chunk_blocks must be positive", ErrInvalidConfig)
	}
	if c.MaxPools <= 0 {
		return fmt.Errorf("%w: max_pools must be positive", ErrInvalidConfig)
	}
	if c.MaxFee > maxFeePips {
		return fmt.Errorf("%w: max_fee %d exceeds %d pips", ErrInvalidConfig, c.MaxFee, maxFeePips)
	}
	if c.MinLiquidity != nil && c.MinLiquidity.Sign() < 0 {
		return fmt.Errorf("%w: min_liquidity must not be negative", ErrInvalidConfig)
	}
	if len(c.Sources) == 0 {
		return fmt.Errorf("%w: sources must not be empty", ErrInvalidConfig)
	}

	seenSources := make(map[Source]struct{}, len(c.Sources))
	for i, source := range c.Sources {
		if !source.Valid() {
			return fmt.Errorf("%w: sources[%d]: unknown source %d", ErrInvalidConfig, i, source)
		}
		if _, ok := seenSources[source]; ok {
			return fmt.Errorf("%w: sources[%d]: duplicate source %s", ErrInvalidConfig, i, source)
		}
		if source.Contract(c.Deployments) == (domain.Address{}) {
			return fmt.Errorf("%w: no %s deployment configured for this chain", ErrInvalidConfig, source)
		}
		if source == SourceUniswapV4 && c.Deployments != nil && c.Deployments.V4StateView == (domain.Address{}) {
			return fmt.Errorf("%w: V4 StateView is not configured", ErrInvalidConfig)
		}
		seenSources[source] = struct{}{}
	}

	seenSeeds := make(map[domain.PoolKey]struct{}, len(c.Seeds))
	for i, seed := range c.Seeds {
		if err := validateSeed(seed, c.Deployments); err != nil {
			return fmt.Errorf("%w: seed_pools[%d]: %w", ErrInvalidConfig, i, err)
		}
		key := seed.PoolKey()
		if _, ok := seenSeeds[key]; ok {
			return fmt.Errorf("%w: seed_pools[%d]: duplicate pool %s", ErrInvalidConfig, i, seed.label())
		}
		seenSeeds[key] = struct{}{}
	}
	return nil
}

// minLiquidity returns the configured floor, treating nil as zero.
func (c Config) minLiquidity() *big.Int {
	if c.MinLiquidity == nil {
		return new(big.Int)
	}
	return c.MinLiquidity
}

func validateSeed(spec Spec, deployments ...*domain.Deployments) error {
	if spec.Address == (domain.Address{}) {
		return fmt.Errorf("%w: zero pool address", ErrInvalidConfig)
	}
	switch spec.Kind {
	case domain.PoolKindV4:
		return validateV4Seed(spec, deployments...)
	case domain.PoolKindV2, domain.PoolKindV3, domain.PoolKindSlipstream, domain.PoolKindCamelotV3, domain.PoolKindVelodromeV2, domain.PoolKindSolidlyV3:
		if spec.PoolID != (domain.Hash{}) || spec.Hooks != (domain.Address{}) {
			return fmt.Errorf("%w: non-v4 pool %s has V4 identity fields",
				ErrInvalidConfig, spec.Address)
		}
		if err := validateSeedTokens(spec); err != nil {
			return err
		}
		if (spec.Fee == 0 && spec.Kind != domain.PoolKindV2 && spec.Kind != domain.PoolKindCamelotV3 && spec.Kind != domain.PoolKindVelodromeV2) || spec.Fee >= maxFeePips {
			return fmt.Errorf("%w: pool %s fee %d pips outside [1, %d)",
				ErrInvalidConfig, spec.Address, spec.Fee, maxFeePips)
		}
		if spec.Kind.UsesTicks() && spec.TickSpacing <= 0 {
			return fmt.Errorf("%w: pool %s has no tick spacing", ErrInvalidConfig, spec.Address)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported pool kind %d", ErrInvalidConfig, spec.Kind)
	}
}

func validateV4Seed(spec Spec, deployments ...*domain.Deployments) error {
	manager := SourceUniswapV4.Contract(deployments...)
	if manager == (domain.Address{}) || spec.Address != manager {
		return fmt.Errorf("%w: v4 pool address %s is not the PoolManager %s",
			ErrInvalidConfig, spec.Address, manager)
	}
	if spec.PoolID == (domain.Hash{}) {
		return fmt.Errorf("%w: v4 pool has a zero pool ID", ErrInvalidConfig)
	}
	if spec.Hooks != (domain.Address{}) {
		return fmt.Errorf("%w: v4 pool %s has non-zero hooks %s; the bootstrap and quote paths reject hooked pools",
			ErrInvalidConfig, spec.label(), spec.Hooks)
	}
	if err := validateSeedTokens(spec); err != nil {
		return err
	}
	if spec.Fee >= maxFeePips {
		return fmt.Errorf("%w: v4 pool %s fee %d pips outside [0, %d)",
			ErrInvalidConfig, spec.label(), spec.Fee, maxFeePips)
	}
	if spec.TickSpacing <= 0 {
		return fmt.Errorf("%w: v4 pool %s has no tick spacing", ErrInvalidConfig, spec.label())
	}
	expected, err := uniswapv4.PoolID(spec.Token0, spec.Token1, spec.Fee, spec.TickSpacing, spec.Hooks)
	if err != nil {
		return fmt.Errorf("%w: v4 pool %s: %v", ErrInvalidConfig, spec.label(), err)
	}
	if expected != spec.PoolID {
		return fmt.Errorf("%w: v4 pool %s pool ID does not match its key (want %s)",
			ErrInvalidConfig, spec.label(), expected.Hex())
	}
	return nil
}

func validateSeedTokens(spec Spec) error {
	// V4 represents native ETH as currency0, so only the other kinds require a
	// non-zero token0 strictly below token1.
	if spec.Token1 == (domain.Address{}) {
		return fmt.Errorf("%w: zero token1", ErrInvalidConfig)
	}
	if spec.Token0 == spec.Token1 {
		return fmt.Errorf("%w: token0 and token1 are equal", ErrInvalidConfig)
	}
	if spec.Kind != domain.PoolKindV4 && spec.Token0 == (domain.Address{}) {
		return fmt.Errorf("%w: zero token0", ErrInvalidConfig)
	}
	return nil
}

type rawConfig struct {
	Emitters []emitterJSON `json:"emitters"`

	InventoryPath string           `json:"inventory_path"`
	Selection     *SelectionPolicy `json:"selection"`

	LookbackBlocks        *uint64   `json:"lookback_blocks"`
	RefreshBlocks         *uint64   `json:"refresh_blocks"`
	ScanChunkBlocks       *uint64   `json:"scan_chunk_blocks"`
	HistoryChunkBlocks    *uint64   `json:"history_chunk_blocks"`
	HistoryChunksPerRound *int      `json:"history_chunks_per_round"`
	MaxPools              *int      `json:"max_pools"`
	MinLiquidity          *string   `json:"min_liquidity"`
	MaxFee                *uint32   `json:"max_fee"`
	QuoteTokens           []string  `json:"quote_tokens"`
	DenyTokenCode         []string  `json:"deny_token_code_selectors"`
	Sources               []string  `json:"sources"`
	SeedPools             []rawSeed `json:"seed_pools"`
}

type rawSeed struct {
	Address     string  `json:"address"`
	Token0      string  `json:"token0"`
	Token1      string  `json:"token1"`
	Kind        string  `json:"kind"`
	PoolID      string  `json:"pool_id"`
	Hooks       string  `json:"hooks"`
	Fee         *uint32 `json:"fee"`
	TickSpacing int32   `json:"tick_spacing"`
}

func (r rawSeed) spec() (Spec, error) {
	kind, err := parseSeedKind(r.Kind)
	if err != nil {
		return Spec{}, err
	}
	fee := uint32(0)
	if r.Fee != nil {
		fee = *r.Fee
	} else if kind == domain.PoolKindV2 {
		return Spec{}, fmt.Errorf("v2 seed requires an explicit fee in pips")
	}
	address, err := domain.ParseAddress(r.Address)
	if err != nil {
		return Spec{}, fmt.Errorf("address: %v", err)
	}
	token0, err := domain.ParseAddress(r.Token0)
	if err != nil {
		return Spec{}, fmt.Errorf("token0: %v", err)
	}
	token1, err := domain.ParseAddress(r.Token1)
	if err != nil {
		return Spec{}, fmt.Errorf("token1: %v", err)
	}
	var poolID domain.Hash
	if r.PoolID != "" {
		poolID, err = domain.ParseHash(r.PoolID)
		if err != nil {
			return Spec{}, fmt.Errorf("pool_id: %v", err)
		}
	}
	var hooks domain.Address
	if r.Hooks != "" {
		hooks, err = domain.ParseAddress(r.Hooks)
		if err != nil {
			return Spec{}, fmt.Errorf("hooks: %v", err)
		}
	}
	return Spec{
		Address:     address,
		Token0:      token0,
		Token1:      token1,
		Kind:        kind,
		PoolID:      poolID,
		Hooks:       hooks,
		Fee:         fee,
		TickSpacing: r.TickSpacing,
	}, nil
}

// parseSeedKind maps a seed kind to the pool kind bootstrap understands. Only
// the kinds discovery can scan are accepted.
func parseSeedKind(text string) (domain.PoolKind, error) {
	switch normalizeName(text) {
	case "velodrome_v2":
		return domain.PoolKindVelodromeV2, nil
	case "v3":
		return domain.PoolKindV3, nil
	case "v2":
		return domain.PoolKindV2, nil
	case "camelot_v3":
		return domain.PoolKindCamelotV3, nil
	case "slipstream":
		return domain.PoolKindSlipstream, nil
	case "solidly_v3":
		return domain.PoolKindSolidlyV3, nil
	case "v4":
		return domain.PoolKindV4, nil
	default:
		return 0, fmt.Errorf("kind %q is not one of v2, v3, camelot_v3, slipstream, solidly_v3, v4", text)
	}
}

func normalizeName(text string) string {
	return strings.ToLower(strings.TrimSpace(text))
}

// DeniedTokenCode reports whether deployed code pushes a denied selector. A
// match inside other push data is possible and only costs one pool.
func (c Config) DeniedTokenCode(code []byte) bool {
	for _, selector := range c.DenyTokenCode {
		if bytes.Contains(code, append([]byte{0x63}, selector[:]...)) {
			return true
		}
	}
	return false
}
