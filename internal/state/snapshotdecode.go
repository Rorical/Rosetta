package state

import (
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func snapshotFromWire(wire snapshotWire) (Snapshot, error) {
	if wire.Version != SnapshotVersion && wire.Version != DirectionalStateVersion && wire.Version != ProtocolStateVersion && wire.Version != OracleStateVersion && wire.Version != MetricStateVersion {
		return Snapshot{}, fmt.Errorf("%w: version %d, want %d", ErrInvalidSnapshot, wire.Version, SnapshotVersion)
	}
	block, err := blockFromWire("block", wire.Block)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{block: block}
	if snap.pools, err = poolsFromWire(wire.Pools, wire.Version); err != nil {
		return Snapshot{}, err
	}
	if snap.deep, err = deepFromWire(wire.Deep, snap.pools); err != nil {
		return Snapshot{}, err
	}
	if snap.dirty, err = dirtyFromWire(wire.Dirty, snap.pools); err != nil {
		return Snapshot{}, err
	}
	if snap.deepStale, err = poolKeySetFromWire(wire.DeepStale, snap.pools, "deep stale entry"); err != nil {
		return Snapshot{}, err
	}
	if err := snap.validate(); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func poolsFromWire(entries []poolWire, version int) (map[domain.PoolKey]pool.State, error) {
	pools := make(map[domain.PoolKey]pool.State, len(entries))
	for _, entry := range entries {
		if entry.Slipstream != nil && (version < OracleStateVersion || domain.PoolKind(entry.Kind) != domain.PoolKindSlipstream) {
			return nil, fmt.Errorf("%w: Slipstream oracle requires matching kind and version 4", ErrInvalidSnapshot)
		}
		if entry.FeeOneForZero != nil && (version < DirectionalStateVersion || domain.PoolKind(entry.Kind) != domain.PoolKindCamelotV3 || *entry.FeeOneForZero >= 1_000_000) {
			return nil, fmt.Errorf("%w: directional fee requires version 2 or later and a valid Camelot pool", ErrInvalidSnapshot)
		}
		if err := validateVelodromeWire(entry, version); err != nil {
			return nil, err
		}
		if (entry.Metric != nil) != (domain.PoolKind(entry.Kind) == domain.PoolKindMetricOmm) || entry.Metric != nil && version < MetricStateVersion {
			return nil, fmt.Errorf("%w: MetricOmm registration requires matching kind and version 5", ErrInvalidSnapshot)
		}
		key, err := poolKeyFromWire(entry.Address, entry.Kind, entry.PoolID, entry.Hooks)
		if err != nil {
			return nil, fmt.Errorf("%w: pool %q: %v", ErrInvalidSnapshot, entry.Address, err)
		}
		if _, exists := pools[key]; exists {
			return nil, fmt.Errorf("%w: duplicate pool %s", ErrInvalidSnapshot, formatPoolKey(key))
		}
		state, err := poolFromWire(key, entry)
		if err != nil {
			return nil, err
		}
		pools[key] = state
	}
	return pools, nil
}

func poolFromWire(key domain.PoolKey, entry poolWire) (pool.State, error) {
	token0, err := domain.ParseAddress(entry.Token0)
	if err != nil {
		return pool.State{}, fmt.Errorf("%w: pool %s token0: %v", ErrInvalidSnapshot, formatPoolKey(key), err)
	}
	token1, err := domain.ParseAddress(entry.Token1)
	if err != nil {
		return pool.State{}, fmt.Errorf("%w: pool %s token1: %v", ErrInvalidSnapshot, formatPoolKey(key), err)
	}
	state := pool.State{
		Key:           key,
		Token0:        token0,
		Token1:        token1,
		Fee:           entry.Fee,
		FeeOneForZero: entry.FeeOneForZero,
		ProtocolFee:   entry.ProtocolFee,
		TickSpacing:   entry.TickSpacing,
		Tick:          entry.Tick,
		UpdatedBlock:  entry.UpdatedBlock,
		Slipstream:    entry.Slipstream,
	}
	if err := slipstream.ValidateState(state.Slipstream); err != nil {
		return pool.State{}, fmt.Errorf("%w: %v", ErrInvalidSnapshot, err)
	}
	if m := entry.Velodrome; m != nil {
		factory, _ := domain.ParseAddress(m.Factory)
		scale0, _ := new(big.Int).SetString(m.Scale0, 10)
		scale1, _ := new(big.Int).SetString(m.Scale1, 10)
		state.Velodrome = &pool.VelodromeState{Factory: factory, Stable: m.Stable, Scale0: scale0, Scale1: scale1}
	}
	if state.Metric, err = metricFromWire(entry.Metric); err != nil {
		return pool.State{}, fmt.Errorf("%w: pool %s: %v", ErrInvalidSnapshot, formatPoolKey(key), err)
	}
	prefix := "pool " + formatPoolKey(key) + " "
	if state.SqrtPriceX96, err = bigDecimal(prefix+"sqrt_price_x96", entry.SqrtPriceX96); err != nil {
		return pool.State{}, err
	}
	if state.Liquidity, err = bigDecimal(prefix+"liquidity", entry.Liquidity); err != nil {
		return pool.State{}, err
	}
	if state.Reserve0, err = bigDecimal(prefix+"reserve0", entry.Reserve0); err != nil {
		return pool.State{}, err
	}
	if state.Reserve1, err = bigDecimal(prefix+"reserve1", entry.Reserve1); err != nil {
		return pool.State{}, err
	}
	return state, nil
}

// poolKeyFromWire builds a key from its wire fields. Address and kind are
// required; pool_id and hooks are optional and decode to the zero values, which
// is what a document written before those fields existed contains.
func poolKeyFromWire(address string, kind uint8, poolID, hooks string) (domain.PoolKey, error) {
	addr, err := domain.ParseAddress(address)
	if err != nil {
		return domain.PoolKey{}, err
	}
	id, err := optionalHash(poolID, "pool id")
	if err != nil {
		return domain.PoolKey{}, err
	}
	hookAddress, err := optionalAddress(hooks, "hooks")
	if err != nil {
		return domain.PoolKey{}, err
	}
	return domain.PoolKey{Address: addr, Kind: domain.PoolKind(kind), PoolID: id, Hooks: hookAddress}, nil
}

func deepFromWire(entries []deepWire, pools map[domain.PoolKey]pool.State) (map[domain.PoolKey]pool.DeepState, error) {
	deep := make(map[domain.PoolKey]pool.DeepState, len(entries))
	for _, entry := range entries {
		key, err := deepKeyFromWire(entry, pools)
		if err != nil {
			return nil, fmt.Errorf("%w: deep %q: %v", ErrInvalidSnapshot, entry.Address, err)
		}
		if _, exists := deep[key]; exists {
			return nil, fmt.Errorf("%w: duplicate deep state for %s", ErrInvalidSnapshot, formatPoolKey(key))
		}
		state, err := deepStateFromWire(key, entry)
		if err != nil {
			return nil, err
		}
		deep[key] = state
	}
	return deep, nil
}

// deepKeyFromWire recovers the key a deep entry belongs to. A version 1
// document has only the address, so the kind comes from the shallow pool set.
func deepKeyFromWire(entry deepWire, pools map[domain.PoolKey]pool.State) (domain.PoolKey, error) {
	candidate, err := poolKeyFromWire(entry.Address, entry.Kind, entry.PoolID, entry.Hooks)
	if err != nil {
		return domain.PoolKey{}, err
	}
	return resolvePoolKey(candidate, pools)
}

// blockFromWire decodes the wire form shared by the chain head and a deep
// state's loaded block.
func blockFromWire(field string, wire blockWire) (domain.BlockRef, error) {
	hash, err := domain.ParseHash(wire.Hash)
	if err != nil {
		return domain.BlockRef{}, fmt.Errorf("%w: %s hash: %v", ErrInvalidSnapshot, field, err)
	}
	parentHash, err := domain.ParseHash(wire.ParentHash)
	if err != nil {
		return domain.BlockRef{}, fmt.Errorf("%w: %s parent hash: %v", ErrInvalidSnapshot, field, err)
	}
	return domain.BlockRef{
		Number:     wire.Number,
		Hash:       hash,
		ParentHash: parentHash,
		Timestamp:  wire.Timestamp,
		L1Block:    wire.L1Block,
	}, nil
}

func deepStateFromWire(key domain.PoolKey, entry deepWire) (pool.DeepState, error) {
	loaded, err := blockFromWire(fmt.Sprintf("deep %s loaded block", formatPoolKey(key)), entry.LoadedBlock)
	if err != nil {
		return pool.DeepState{}, err
	}
	state := pool.DeepState{
		Bitmap:      make(map[int16]*big.Int, len(entry.Bitmap)),
		Ticks:       make(map[int32]pool.TickInfo, len(entry.Ticks)),
		WindowMin:   entry.WindowMin,
		WindowMax:   entry.WindowMax,
		LoadedBlock: loaded,
	}
	for _, word := range entry.Bitmap {
		if _, exists := state.Bitmap[word.Word]; exists {
			return pool.DeepState{}, fmt.Errorf("%w: deep %s duplicate bitmap word %d", ErrInvalidSnapshot, formatPoolKey(key), word.Word)
		}
		bits, err := parseBitmapWord(word.Bits)
		if err != nil {
			return pool.DeepState{}, fmt.Errorf("deep %s word %d: %w", formatPoolKey(key), word.Word, err)
		}
		state.Bitmap[word.Word] = bits
	}
	for _, tick := range entry.Ticks {
		if _, exists := state.Ticks[tick.Tick]; exists {
			return pool.DeepState{}, fmt.Errorf("%w: deep %s duplicate tick %d", ErrInvalidSnapshot, formatPoolKey(key), tick.Tick)
		}
		prefix := fmt.Sprintf("deep %s tick %d ", formatPoolKey(key), tick.Tick)
		gross, err := bigDecimal(prefix+"liquidity_gross", tick.LiquidityGross)
		if err != nil {
			return pool.DeepState{}, err
		}
		net, err := bigDecimal(prefix+"liquidity_net", tick.LiquidityNet)
		if err != nil {
			return pool.DeepState{}, err
		}
		state.Ticks[tick.Tick] = pool.TickInfo{LiquidityGross: gross, LiquidityNet: net}
	}
	return state, nil
}

func dirtyFromWire(entries []string, pools map[domain.PoolKey]pool.State) (map[domain.PoolKey]struct{}, error) {
	return poolKeySetFromWire(entries, pools, "dirty entry")
}

// poolKeySetFromWire decodes a list of encoded pool keys and resolves each
// against the pool set. field names the list in error messages. An absent list
// decodes to an empty set, which keeps documents written before the field
// existed valid.
func poolKeySetFromWire(entries []string, pools map[domain.PoolKey]pool.State, field string) (map[domain.PoolKey]struct{}, error) {
	keys := make(map[domain.PoolKey]struct{}, len(entries))
	for _, raw := range entries {
		candidate, _, err := decodePoolKeyString(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrInvalidSnapshot, field, err)
		}
		key, err := resolvePoolKey(candidate, pools)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrInvalidSnapshot, field, err)
		}
		if _, exists := keys[key]; exists {
			return nil, fmt.Errorf("%w: duplicate %s %s", ErrInvalidSnapshot, field, formatPoolKey(key))
		}
		keys[key] = struct{}{}
	}
	return keys, nil
}

// optionalHash parses a hex hash where an empty string means the zero hash.
func optionalHash(raw, field string) (domain.Hash, error) {
	if raw == "" {
		return domain.Hash{}, nil
	}
	hash, err := domain.ParseHash(raw)
	if err != nil {
		return domain.Hash{}, fmt.Errorf("%s %q: %v", field, raw, err)
	}
	return hash, nil
}

// optionalAddress parses a hex address where an empty string means the zero
// address.
func optionalAddress(raw, field string) (domain.Address, error) {
	if raw == "" {
		return domain.Address{}, nil
	}
	address, err := domain.ParseAddress(raw)
	if err != nil {
		return domain.Address{}, fmt.Errorf("%s %q: %v", field, raw, err)
	}
	return address, nil
}

// bigDecimal parses a signed decimal string. The empty string encodes nil, so
// an absent value survives a round trip as absent.
func bigDecimal(field, raw string) (*big.Int, error) {
	if raw == "" {
		return nil, nil
	}
	value, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("%w: %s: invalid decimal %q", ErrInvalidSnapshot, field, raw)
	}
	return value, nil
}

// parseBitmapWord decodes a fixed-width 256-bit word. Requiring the exact width
// keeps the encoding canonical and rejects signs or short values.
func parseBitmapWord(bits string) (*big.Int, error) {
	if len(bits) != wordHexLen {
		return nil, fmt.Errorf("%w: bitmap word wants %d hex characters, got %d", ErrInvalidSnapshot, wordHexLen, len(bits))
	}
	raw, err := hex.DecodeString(bits)
	if err != nil {
		return nil, fmt.Errorf("%w: bitmap word: %v", ErrInvalidSnapshot, err)
	}
	return new(big.Int).SetBytes(raw), nil
}
