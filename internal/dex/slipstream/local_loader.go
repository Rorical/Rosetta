package slipstream

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"golang.org/x/crypto/sha3"
)

// LocalStateCaller binds code identity and oracle reads to one execution
// snapshot. Generic callers retain their existing RPC fee-only behavior.
type LocalStateCaller interface {
	pool.ContractCaller
	FeeStateBlock() domain.BlockRef
	CodeHash(context.Context, domain.Address) (domain.Hash, error)
}

// These runtime hashes identify verified deployments of the factory and its
// DynamicSwapFeeHook, DiscountRegistry and PriorityFeeMevTaxModule. A changed
// implementation is not inferred from ABI compatibility.
//
// Arbitrum One's CLFactory has the OP source with other immutables. Its hook
// is ArbDynamicSwapFeeHook, the OP DynamicSwapFeeHook with one override:
// _blockNumber() reads ArbSys.arbBlockNumber(), since block.number is the L1
// block there. The first-swap fee cache therefore keys on the L2 block, the
// number AdvanceTime follows (verified against Sourcify full matches).
var (
	supportedFactories = map[string]bool{
		"0x68e96345f7fdc80c927fd25a04f6f5846cdd00173f031eae7517722a67103c7a": true, // OP Mainnet
		"0x4cb82764ba4fd4a80c986952190a6052747a816a6dc5c7be3fc3a7a7b1bd0af4": true, // Arbitrum One
	}
	supportedHooks = map[string]bool{
		"0x3064a620b37842c2e0ff3ea7619d7eac49197bd4b247955e7250250dc1ae1239": true, // OP Mainnet DynamicSwapFeeHook
		"0xbc9d92d8836ab8e1216cec5321922fe861b31bc2aa4906bc934500aa0d2dad47": true, // Arbitrum One ArbDynamicSwapFeeHook
	}
)

const (
	supportedRegistry = "0x0b972ebfbde67027f54fe4cef10c5c1ec2a2ada1b1f1c532a5b73fd6f0f3b1cf"
	supportedTax      = "0xfb10c802d480cf1d35d1878c7dc9b9e12a54b600f66da776991554ff0c2f9657"
)

// loadLocal seeds the retained oracle window once. Subsequent blocks follow
// logs locally; config changes invalidate the model and request a new seed.
func loadLocal(ctx context.Context, c LocalStateCaller, p *pool.State, block string) error {
	ref := c.FeeStateBlock()
	if ref.Timestamp == 0 {
		return nil
	}
	factoryWords, err := readWords(ctx, c, p.Key.Address, "factory()", nil, block, 1)
	if err != nil {
		return err
	}
	factory := wordAddress(factoryWords[0])
	hash, err := c.CodeHash(ctx, factory)
	if err != nil {
		return err
	}
	if !supportedFactories[hash.Hex()] {
		if moduleHash, ok := legacyModules[hash.Hex()]; ok {
			return loadLegacy(ctx, c, p, factory, moduleHash, block)
		}
		return nil
	}
	m := &pool.SlipstreamState{Factory: factory, Block: ref.Number, Timestamp: uint32(ref.Timestamp)}
	p.Slipstream = m
	hook, err := readWords(ctx, c, factory, "getPoolSwapHook(address)", addressWord(p.Key.Address), block, 1)
	if err != nil {
		return err
	}
	m.Hook = wordAddress(hook[0])
	hash, err = c.CodeHash(ctx, m.Hook)
	if err != nil {
		return err
	}
	if !supportedHooks[hash.Hex()] {
		m.UnavailableReason = "unsupported_hook_code"
		return nil
	}
	slot, err := readWords(ctx, c, p.Key.Address, "slot0()", nil, block, 6)
	if err != nil {
		return err
	}
	m.ObservationIndex = uint16(wordUint(slot[2]))
	m.Cardinality = uint16(wordUint(slot[3]))
	m.CardinalityNext = uint16(wordUint(slot[4]))
	// A fee-only canonical refresh starts from an identity-only state.
	p.Tick = int32(decodeSignedWord(slot[1], 24))
	p.SqrtPriceX96 = new(big.Int).SetBytes(slot[0])
	config, err := readWords(ctx, c, m.Hook, "dynamicFeeConfig(address)", addressWord(p.Key.Address), block, 5)
	if err != nil {
		return err
	}
	m.BaseFee = uint32(wordUint(config[0]))
	m.FeeCap = uint32(wordUint(config[1]))
	m.ScalingFactor = wordUint(config[2])
	m.InitialFeeEnabled = wordUint(config[3]) != 0
	m.InitialFee = uint32(wordUint(config[4]))
	getters := []string{"defaultScalingFactor()", "defaultFeeCap()", "secondsAgo()", "mevTaxModule()"}
	values := make([][]byte, len(getters))
	for i, getter := range getters {
		words, e := readWords(ctx, c, m.Hook, getter, nil, block, 1)
		if e != nil {
			return e
		}
		values[i] = words[0]
	}
	m.DefaultScalingFactor = wordUint(values[0])
	m.DefaultFeeCap = uint32(wordUint(values[1]))
	m.SecondsAgo = uint32(wordUint(values[2]))
	m.TaxModule = wordAddress(values[3])
	spacing, err := readWords(ctx, c, factory, "tickSpacingToFee(int24)", uintWord(uint64(p.TickSpacing)), block, 1)
	if err != nil {
		return err
	}
	m.SpacingFee = uint32(wordUint(spacing[0]))
	registry, err := readWords(ctx, c, factory, "discountRegistry()", nil, block, 1)
	if err != nil {
		return err
	}
	m.Registry = wordAddress(registry[0])
	if m.Registry != (domain.Address{}) {
		hash, err = c.CodeHash(ctx, m.Registry)
		if err != nil {
			return err
		}
		if hash.Hex() != supportedRegistry {
			m.UnavailableReason = "unsupported_discount_registry_code"
			return nil
		}
	}
	if m.TaxModule != (domain.Address{}) {
		hash, err = c.CodeHash(ctx, m.TaxModule)
		if err != nil {
			return err
		}
		if hash.Hex() != supportedTax {
			m.UnavailableReason = "unsupported_tax_code"
			return nil
		}
		tax, err := readWords(ctx, c, m.TaxModule, "priorityFeeMultiplier()", nil, block, 1)
		if err != nil {
			return err
		}
		m.TaxMultiplier = wordUint(tax[0])
		tax, err = readWords(ctx, c, m.TaxModule, "minThreshold()", nil, block, 1)
		if err != nil {
			return err
		}
		m.TaxMinimum = new(big.Int).SetBytes(tax[0]).String()
		tax, err = readWords(ctx, c, m.TaxModule, "baseFeeFactor()", nil, block, 1)
		if err != nil {
			return err
		}
		m.TaxBaseFactor = new(big.Int).SetBytes(tax[0]).String()
	}
	cached, err := readWords(ctx, c, m.Hook, "blockFee(address,uint256)", append(addressWord(p.Key.Address), uintWord(ref.Number)...), block, 1)
	if err != nil {
		return err
	}
	m.BlockFee = uint32(wordUint(cached[0]))
	if err = loadOracle(ctx, c, p.Key.Address, m, block); err != nil {
		return err
	}
	m.Valid = true
	if err = ValidateState(m); err != nil {
		return err
	}
	fee, err := LocalFee(*p)
	if err != nil {
		return err
	}
	// fee() uses a zero caller/origin and zero gas price, matching this model.
	// A mismatch rejects the model rather than training on an invented fee.
	if fee != p.Fee {
		m.Valid = false
		m.UnavailableReason = fmt.Sprintf("local_fee_%d_canonical_fee_%d", fee, p.Fee)
		return nil
	}
	return nil
}

func loadOracle(ctx context.Context, c pool.ContractCaller, address domain.Address, m *pool.SlipstreamState, block string) error {
	if m.Cardinality == 0 || m.ObservationIndex >= m.Cardinality {
		return fmt.Errorf("%w: oracle cardinality", pool.ErrInvalidState)
	}
	entries := make([]pool.OracleObservation, 0, min(int(m.Cardinality), int(m.SecondsAgo)+2))
	done := false
	for offset := 0; offset < int(m.Cardinality) && !done; {
		count := min(32, int(m.Cardinality)-offset)
		calls := make([]pool.ContractCall, count)
		for i := range calls {
			index := (int(m.ObservationIndex) - offset - i + int(m.Cardinality)) % int(m.Cardinality)
			calls[i] = pool.ContractCall{To: address, Data: abiCall("observations(uint256)", uintWord(uint64(index))), Block: block}
		}
		results, err := callMany(ctx, c, calls)
		if err != nil {
			return err
		}
		for i, out := range results {
			words, err := decodeWords(out, 4)
			if err != nil {
				return err
			}
			if wordUint(words[3]) == 0 {
				m.Complete = true
				done = true
				break
			}
			index := (int(m.ObservationIndex) - offset - i + int(m.Cardinality)) % int(m.Cardinality)
			entry := pool.OracleObservation{Index: uint16(index), Timestamp: uint32(wordUint(words[0])), TickCumulative: decodeSignedWord(words[1], 56)}
			entries = append(entries, entry)
			if len(entries) == int(m.Cardinality) {
				m.Complete = true
				done = true
				break
			}
			if m.Timestamp-entry.Timestamp >= m.SecondsAgo {
				done = true
				break
			}
		}
		offset += count
	}
	slices.Reverse(entries)
	m.Observations = pool.NewOracleHistory(entries)
	return nil
}

func callMany(ctx context.Context, c pool.ContractCaller, calls []pool.ContractCall) ([]string, error) {
	if batch, ok := c.(pool.BatchContractCaller); ok {
		return batch.CallContracts(ctx, calls)
	}
	results := make([]string, len(calls))
	for i, call := range calls {
		if err := c.CallContract(ctx, call.To, call.Data, call.Block, &results[i]); err != nil {
			return nil, err
		}
	}
	return results, nil
}
func readWords(ctx context.Context, c pool.ContractCaller, to domain.Address, signature string, args []byte, block string, n int) ([][]byte, error) {
	var out string
	if err := c.CallContract(ctx, to, abiCall(signature, args), block, &out); err != nil {
		return nil, fmt.Errorf("slipstream %s: %w", signature, err)
	}
	return decodeWords(out, n)
}
func decodeWords(out string, n int) ([][]byte, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(out, "0x"))
	if err != nil || len(raw) != 32*n {
		return nil, fmt.Errorf("%w: expected %d ABI words", ErrInvalidResponse, n)
	}
	words := make([][]byte, n)
	for i := range words {
		words[i] = raw[i*32 : (i+1)*32]
	}
	return words, nil
}
func abiCall(signature string, args []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(signature))
	return append(h.Sum(nil)[:4:4], args...)
}
func uintWord(n uint64) []byte                   { return new(big.Int).SetUint64(n).FillBytes(make([]byte, 32)) }
func addressWord(a domain.Address) []byte        { out := make([]byte, 32); copy(out[12:], a[:]); return out }
func wordAddress(w []byte) domain.Address        { var a domain.Address; copy(a[:], w[12:]); return a }
func wordUint(w []byte) uint64                   { return new(big.Int).SetBytes(w).Uint64() }
func decodeSignedWord(w []byte, bits uint) int64 { return int64(wordUint(w)<<(64-bits)) >> (64 - bits) }
