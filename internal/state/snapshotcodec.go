package state

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// SnapshotVersion is the baseline JSON schema. Directional pools require
// DirectionalStateVersion; all other unsupported versions are rejected.
const SnapshotVersion = 1

// DirectionalStateVersion is required when the reverse fee carries independent
// information. Older binaries reject this version instead of silently losing it.
const DirectionalStateVersion = 2

// ProtocolStateVersion adds the metadata required to price non-Uniswap curves.
const ProtocolStateVersion = 3

// OracleStateVersion preserves the local Slipstream oracle and block fee cache.
const OracleStateVersion = 4

// MetricStateVersion adds the registration of MetricOmm pools.
const MetricStateVersion = 5

// wordHexLen is the fixed width of an encoded tick bitmap word: 256 bits.
const wordHexLen = 64

// snapshotWire mirrors a Snapshot as JSON. Lists carry map contents so field
// order is fixed and the same store always encodes to the same bytes.
type snapshotWire struct {
	Version   int        `json:"version"`
	Block     blockWire  `json:"block"`
	Pools     []poolWire `json:"pools"`
	Deep      []deepWire `json:"deep"`
	Dirty     []string   `json:"dirty"`
	DeepStale []string   `json:"deep_stale,omitempty"`
}

type blockWire struct {
	Number     uint64 `json:"number"`
	Hash       string `json:"hash"`
	ParentHash string `json:"parent_hash"`
	Timestamp  uint64 `json:"timestamp"`
	L1Block    uint64 `json:"l1_block"`
}

// poolWire is one shallow pool. pool_id and hooks are omitted for every kind
// except V4, and protocol_fee is omitted when it is zero, so a document written
// before those fields existed still decodes: the missing values are the zero
// values.
type poolWire struct {
	Address       string                `json:"address"`
	Kind          uint8                 `json:"kind"`
	PoolID        string                `json:"pool_id,omitempty"`
	Hooks         string                `json:"hooks,omitempty"`
	Token0        string                `json:"token0"`
	Token1        string                `json:"token1"`
	Fee           uint32                `json:"fee"`
	FeeOneForZero *uint32               `json:"fee_one_for_zero,omitempty"`
	ProtocolFee   uint32                `json:"protocol_fee,omitempty"`
	TickSpacing   int32                 `json:"tick_spacing"`
	SqrtPriceX96  string                `json:"sqrt_price_x96"`
	Tick          int32                 `json:"tick"`
	Liquidity     string                `json:"liquidity"`
	Reserve0      string                `json:"reserve0"`
	Reserve1      string                `json:"reserve1"`
	UpdatedBlock  uint64                `json:"updated_block"`
	Slipstream    *pool.SlipstreamState `json:"slipstream,omitempty"`
	Velodrome     *velodromeWire        `json:"velodrome,omitempty"`
	Metric        *metricWire           `json:"metric,omitempty"`
}

type velodromeWire struct {
	Factory string `json:"factory"`
	Stable  bool   `json:"stable"`
	Scale0  string `json:"scale0"`
	Scale1  string `json:"scale1"`
}

// deepWire is one pool's tick-level state. The key fields repeat the shallow
// entry's identity: a version 1 document has only the address, so the decoder
// resolves a missing kind against the shallow set. window_min and window_max
// are optional; a document written before the window existed, or a deep state
// with an unknown window, decodes them as zero.
type deepWire struct {
	Address     string       `json:"address"`
	Kind        uint8        `json:"kind,omitempty"`
	PoolID      string       `json:"pool_id,omitempty"`
	Hooks       string       `json:"hooks,omitempty"`
	WindowMin   int32        `json:"window_min,omitempty"`
	WindowMax   int32        `json:"window_max,omitempty"`
	LoadedBlock blockWire    `json:"loaded_block"`
	Bitmap      []bitmapWire `json:"bitmap"`
	Ticks       []tickWire   `json:"ticks"`
}

type bitmapWire struct {
	Word int16  `json:"word"`
	Bits string `json:"bits"`
}

type tickWire struct {
	Tick           int32  `json:"tick"`
	LiquidityGross string `json:"liquidity_gross"`
	LiquidityNet   string `json:"liquidity_net"`
}

// EncodeSnapshot returns the store as deterministic JSON without touching the
// undo state: the open frame and history survive, so a periodic persistence
// write does not shrink the reorg window. Use Snapshot for an explicit
// rollback checkpoint. Pools, deep state, bitmap words, ticks, dirty markers,
// and stale markers are sorted, and big integers use empty strings for nil, so
// encoding the same store twice produces identical bytes.
func (s *Store) EncodeSnapshot() ([]byte, error) {
	return s.Checkpoint().Encode()
}

// Encode returns the bytes EncodeSnapshot produced when the checkpoint was
// taken. A checkpoint is an independent copy, so encoding can run away from
// the goroutine that owns the store.
func (snap Snapshot) Encode() ([]byte, error) {
	data, err := json.Marshal(snapshotToWire(snap))
	if err != nil {
		return nil, fmt.Errorf("state: encode snapshot: %w", err)
	}
	return data, nil
}

// LoadSnapshot replaces the store contents with the snapshot in data. A
// successful load drops any open undo frame, because a restored store has
// nothing to roll back. Invalid data returns an error wrapping
// ErrInvalidSnapshot and leaves the store, including an open frame, unchanged.
func (s *Store) LoadSnapshot(data []byte) error {
	var wire snapshotWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrInvalidSnapshot, err)
	}
	snap, err := snapshotFromWire(wire)
	if err != nil {
		return err
	}
	return s.Restore(snap)
}

func snapshotToWire(snap Snapshot) snapshotWire {
	wire := snapshotWire{
		Version:   SnapshotVersion,
		Block:     blockToWire(snap.block),
		Pools:     make([]poolWire, 0, len(snap.pools)),
		Deep:      make([]deepWire, 0, len(snap.deep)),
		Dirty:     make([]string, 0, len(snap.dirty)),
		DeepStale: make([]string, 0, len(snap.deepStale)),
	}
	for _, key := range sortedPoolKeys(snap.pools) {
		wire.Pools = append(wire.Pools, poolToWire(key, snap.pools[key]))
		if snap.pools[key].FeeOneForZero != nil {
			wire.Version = max(wire.Version, DirectionalStateVersion)
		}
		if snap.pools[key].Velodrome != nil {
			wire.Version = max(wire.Version, ProtocolStateVersion)
		}
	}
	for _, p := range snap.pools {
		if p.Slipstream != nil {
			wire.Version = max(wire.Version, OracleStateVersion)
		}
		if p.Metric != nil {
			wire.Version = MetricStateVersion
		}
	}
	for _, key := range sortedPoolKeys(snap.deep) {
		wire.Deep = append(wire.Deep, deepToWire(key, snap.deep[key]))
	}
	for _, key := range sortedPoolKeys(snap.dirty) {
		wire.Dirty = append(wire.Dirty, encodePoolKeyString(key))
	}
	for _, key := range sortedPoolKeys(snap.deepStale) {
		wire.DeepStale = append(wire.DeepStale, encodePoolKeyString(key))
	}
	return wire
}

func poolToWire(key domain.PoolKey, state pool.State) poolWire {
	wire := poolWire{
		Address:       key.Address.Hex(),
		Kind:          uint8(key.Kind),
		Token0:        state.Token0.Hex(),
		Token1:        state.Token1.Hex(),
		Fee:           state.Fee,
		FeeOneForZero: state.FeeOneForZero,
		ProtocolFee:   state.ProtocolFee,
		TickSpacing:   state.TickSpacing,
		SqrtPriceX96:  decimalString(state.SqrtPriceX96),
		Tick:          state.Tick,
		Liquidity:     decimalString(state.Liquidity),
		Reserve0:      decimalString(state.Reserve0),
		Reserve1:      decimalString(state.Reserve1),
		UpdatedBlock:  state.UpdatedBlock,
		Slipstream:    state.Slipstream,
	}
	if m := state.Velodrome; m != nil {
		wire.Velodrome = &velodromeWire{Factory: m.Factory.Hex(), Stable: m.Stable, Scale0: decimalString(m.Scale0), Scale1: decimalString(m.Scale1)}
	}
	wire.Metric = metricToWire(state.Metric)
	if key.PoolID != (domain.Hash{}) {
		wire.PoolID = key.PoolID.Hex()
	}
	if key.Hooks != (domain.Address{}) {
		wire.Hooks = key.Hooks.Hex()
	}
	return wire
}

func blockToWire(ref domain.BlockRef) blockWire {
	return blockWire{
		Number:     ref.Number,
		Hash:       ref.Hash.Hex(),
		ParentHash: ref.ParentHash.Hex(),
		Timestamp:  ref.Timestamp,
		L1Block:    ref.L1Block,
	}
}

func deepToWire(key domain.PoolKey, deep pool.DeepState) deepWire {
	wire := deepWire{
		Address:     key.Address.Hex(),
		Kind:        uint8(key.Kind),
		WindowMin:   deep.WindowMin,
		WindowMax:   deep.WindowMax,
		LoadedBlock: blockToWire(deep.LoadedBlock),
		Bitmap:      make([]bitmapWire, 0, len(deep.Bitmap)),
		Ticks:       make([]tickWire, 0, len(deep.Ticks)),
	}
	if key.PoolID != (domain.Hash{}) {
		wire.PoolID = key.PoolID.Hex()
	}
	if key.Hooks != (domain.Address{}) {
		wire.Hooks = key.Hooks.Hex()
	}
	for _, word := range sortedBitmapWords(deep.Bitmap) {
		wire.Bitmap = append(wire.Bitmap, bitmapWire{Word: word, Bits: formatBitmapWord(deep.Bitmap[word])})
	}
	for _, tick := range sortedTicks(deep.Ticks) {
		info := deep.Ticks[tick]
		wire.Ticks = append(wire.Ticks, tickWire{
			Tick:           tick,
			LiquidityGross: decimalString(info.LiquidityGross),
			LiquidityNet:   decimalString(info.LiquidityNet),
		})
	}
	return wire
}

func decimalString(value *big.Int) string {
	if value == nil {
		return ""
	}
	return value.String()
}

// formatBitmapWord renders a bitmap word as 64 lowercase hex characters. A nil
// word means a zero word, matching the quote code's "missing word is zero". A
// value that does not fit keeps its extra digits, so reloading the snapshot
// rejects the corruption instead of truncating it.
func formatBitmapWord(word *big.Int) string {
	if word == nil {
		return strings.Repeat("0", wordHexLen)
	}
	digits := word.Text(16)
	if len(digits) >= wordHexLen {
		return digits
	}
	return strings.Repeat("0", wordHexLen-len(digits)) + digits
}

func sortedBitmapWords(bitmap map[int16]*big.Int) []int16 {
	words := make([]int16, 0, len(bitmap))
	for word := range bitmap {
		words = append(words, word)
	}
	slices.Sort(words)
	return words
}

func sortedTicks(ticks map[int32]pool.TickInfo) []int32 {
	ordered := make([]int32, 0, len(ticks))
	for tick := range ticks {
		ordered = append(ordered, tick)
	}
	slices.Sort(ordered)
	return ordered
}

func sortedPoolKeys[V any](m map[domain.PoolKey]V) []domain.PoolKey {
	keys := make([]domain.PoolKey, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return keys
}
