// Package catalog resolves an explicit token/factory universe at one execution
// snapshot. Pool addresses and immutable metadata come from verified factories.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"slices"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

type Factory struct {
	Address domain.Address
	Kind    domain.PoolKind
	Fees    []uint32
}
type Config struct {
	ChainID   uint64
	Tokens    []domain.Address
	Factories []Factory
	V4        *V4Config
	// MetricOmm lists MetricOmmPools by address (see MetricPool).
	MetricOmm []MetricPool
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var wire struct {
		V4        *v4JSON  `json:"v4"`
		ChainID   uint64   `json:"chain_id"`
		Tokens    []string `json:"tokens"`
		Factories []struct {
			Address string   `json:"address"`
			Kind    string   `json:"kind"`
			Fees    []uint32 `json:"fees"`
		} `json:"factories"`
		MetricOmm []metricJSON `json:"metric_omm"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return Config{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Config{}, fmt.Errorf("catalog: trailing JSON")
	}
	result := Config{ChainID: wire.ChainID}
	if wire.V4 != nil {
		var err error
		result.V4, err = wire.V4.decode()
		if err != nil {
			return Config{}, err
		}
	}
	for _, text := range wire.Tokens {
		address, err := domain.ParseAddress(text)
		if err != nil {
			return Config{}, err
		}
		result.Tokens = append(result.Tokens, address)
	}
	for _, entry := range wire.Factories {
		address, err := domain.ParseAddress(entry.Address)
		if err != nil {
			return Config{}, err
		}
		factory := Factory{Address: address, Fees: entry.Fees}
		switch entry.Kind {
		case "velodrome_v2":
			factory.Kind = domain.PoolKindVelodromeV2
		case "v2":
			factory.Kind = domain.PoolKindV2
		case "v3":
			factory.Kind = domain.PoolKindV3
		case "slipstream":
			factory.Kind = domain.PoolKindSlipstream
		default:
			return Config{}, fmt.Errorf("catalog: unsupported factory kind %q", entry.Kind)
		}
		result.Factories = append(result.Factories, factory)
	}
	for _, entry := range wire.MetricOmm {
		registered, err := entry.decode()
		if err != nil {
			return Config{}, err
		}
		result.MetricOmm = append(result.MetricOmm, registered)
	}
	return result, result.validate()
}
func (c Config) validate() error {
	if c.ChainID == 0 || len(c.Tokens) < 2 || len(c.Tokens) > 32 || (len(c.Factories) == 0 && c.V4 == nil && len(c.MetricOmm) == 0) || len(c.Factories) > 16 {
		return fmt.Errorf("catalog: invalid chain or universe size")
	}
	seen := make(map[domain.Address]bool)
	for _, token := range c.Tokens {
		if (token == (domain.Address{}) && c.V4 == nil) || seen[token] {
			return fmt.Errorf("catalog: duplicate or zero token")
		}
		seen[token] = true
	}
	clear(seen)
	for _, factory := range c.Factories {
		if factory.Address == (domain.Address{}) || seen[factory.Address] {
			return fmt.Errorf("catalog: duplicate or zero factory")
		}
		seen[factory.Address] = true
		if (factory.Kind == domain.PoolKindSlipstream || factory.Kind == domain.PoolKindVelodromeV2) && len(factory.Fees) != 0 {
			return fmt.Errorf("catalog: Slipstream uses on-chain tick spacings")
		}
		if factory.Kind != domain.PoolKindV2 && factory.Kind != domain.PoolKindV3 && factory.Kind != domain.PoolKindSlipstream && factory.Kind != domain.PoolKindVelodromeV2 {
			return fmt.Errorf("catalog: unsupported factory")
		}
		if factory.Kind == domain.PoolKindV3 && (len(factory.Fees) == 0 || len(factory.Fees) > 32) {
			return fmt.Errorf("catalog: V3 fee tiers required")
		}
		if factory.Kind == domain.PoolKindV2 && len(factory.Fees) != 1 {
			return fmt.Errorf("catalog: V2 requires one fixed fee")
		}
		fees := make(map[uint32]bool)
		for _, fee := range factory.Fees {
			if fee >= 1_000_000 || fees[fee] {
				return fmt.Errorf("catalog: invalid or duplicate fee tier")
			}
			fees[fee] = true
		}
	}
	if err := validateMetric(c.MetricOmm); err != nil {
		return err
	}
	if c.V4 != nil {
		return c.V4.validate()
	}
	return nil
}

// Resolve never searches by symbol. Factory pools are never taken by address:
// each is checked against factory(), tokens, fee/spacing and live liquidity.
// MetricOmm pools are the exception, named by address and checked against
// their own immutables (resolveMetric).
func Resolve(ctx context.Context, reader execution.StateReader, cfg Config) ([]bootstrap.Spec, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if reader.Position().Chain.ChainID != cfg.ChainID {
		return nil, fmt.Errorf("catalog: chain mismatch")
	}
	for _, token := range cfg.Tokens {
		if token == (domain.Address{}) {
			continue
		}
		if err := checkCode(ctx, reader, token); err != nil {
			return nil, err
		}
	}
	tokens := slices.Clone(cfg.Tokens)
	slices.SortFunc(tokens, compareAddress)
	var pools []bootstrap.Spec
	seen := make(map[domain.Address]bool)
	for _, factory := range cfg.Factories {
		if err := checkCode(ctx, reader, factory.Address); err != nil {
			return nil, err
		}
		tiers, err := factoryTiers(ctx, reader, factory)
		if err != nil {
			return nil, err
		}
		for i, token0 := range tokens {
			if token0 == (domain.Address{}) {
				continue
			}
			for _, token1 := range tokens[i+1:] {
				found, err := resolvePair(ctx, reader, factory, token0, token1, tiers, seen)
				if err != nil {
					return nil, err
				}
				pools = append(pools, found...)
			}
		}
	}
	if cfg.V4 != nil {
		found, err := resolveV4(ctx, reader, *cfg.V4, tokens)
		if err != nil {
			return nil, err
		}
		pools = append(pools, found...)
	}
	if len(cfg.MetricOmm) > 0 {
		found, err := resolveMetric(ctx, reader, cfg.MetricOmm, tokens)
		if err != nil {
			return nil, err
		}
		pools = append(pools, found...)
	}
	if len(pools) == 0 {
		return nil, fmt.Errorf("catalog: no active pools in selected universe")
	}
	slices.SortFunc(pools, func(a, b bootstrap.Spec) int {
		if cmp := compareAddress(a.Address, b.Address); cmp != 0 {
			return cmp
		}
		return bytes.Compare(a.PoolID[:], b.PoolID[:])
	})
	return pools, nil
}
func checkCode(ctx context.Context, reader execution.StateReader, address domain.Address) error {
	hash, err := reader.CodeHash(ctx, address)
	if err != nil {
		return err
	}
	if hash == (domain.Hash{}) {
		return fmt.Errorf("catalog: no contract at %s", address.Hex())
	}
	return nil
}
func selector(signature string) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(signature))
	return h.Sum(nil)[:4]
}
func callData(signature string, a, b domain.Address, tier uint64) []byte {
	data := make([]byte, 100)
	copy(data, selector(signature))
	copy(data[16:36], a[:])
	copy(data[48:68], b[:])
	new(big.Int).SetUint64(tier).FillBytes(data[68:])
	return data
}
func readAddress(ctx context.Context, reader execution.StateReader, target domain.Address, input []byte) (domain.Address, error) {
	raw, err := reader.ReadContract(ctx, target, input)
	if err != nil {
		return domain.Address{}, err
	}
	if len(raw) != 32 || !bytes.Equal(raw[:12], make([]byte, 12)) {
		return domain.Address{}, fmt.Errorf("catalog: invalid ABI address")
	}
	var address domain.Address
	copy(address[:], raw[12:])
	return address, nil
}
func readWord(ctx context.Context, reader execution.StateReader, target domain.Address, input []byte) (*big.Int, error) {
	raw, err := reader.ReadContract(ctx, target, input)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("catalog: invalid ABI word")
	}
	return new(big.Int).SetBytes(raw), nil
}

func compareAddress(a, b domain.Address) int { return bytes.Compare(a[:], b[:]) }
