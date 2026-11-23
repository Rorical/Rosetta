package remote

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
)

// quietStoreFormat 2 names a model by its pool (a V4 pool by the PoolManager
// and its ID) and its shape. A file of another format is ignored: the models
// are read again, as on a first start.
const quietStoreFormat = 2

// quietReadyWait bounds how long sends wait for the word models after a start
// without saved ones: a first pass over the catalog takes two to three
// minutes, and a pool that cannot be read must not stop sending for good.
const quietReadyWait = 5 * time.Minute

// quietFile is the word models at the applied block they stand at. They
// continue only from that block's child: observeBlock drops them on any
// other, so a file older than the pool snapshot costs nothing but the models.
type quietFile struct {
	Format    int          `json:"format"`
	Block     uint64       `json:"block"`
	Timestamp uint64       `json:"timestamp"`
	Models    []quietModel `json:"models"`
	// Entries are the conditions that were read, each for the pool state it
	// was read against; Layouts are the slots found for each pool.
	Entries []quietSavedEntry  `json:"entries,omitempty"`
	Layouts []quietSavedLayout `json:"layouts,omitempty"`
}

type quietSavedPool struct {
	Kind    domain.PoolKind `json:"kind"`
	Address string          `json:"address"`
	PoolID  string          `json:"pool_id,omitempty"`
	Hooks   string          `json:"hooks,omitempty"`
}

type quietSavedEntry struct {
	Pool    quietSavedPool `json:"pool"`
	Updated uint64         `json:"updated"`
	Block   uint64         `json:"block"`
	Slots   [][2]string    `json:"slots"` // slot, value
	// The state the words were checked against (sameState).
	Price     string `json:"price,omitempty"`
	Liquidity string `json:"liquidity,omitempty"`
	Reserve0  string `json:"reserve0,omitempty"`
	Reserve1  string `json:"reserve1,omitempty"`
}

type quietSavedLayout struct {
	Pool  quietSavedPool `json:"pool"`
	Slots []string       `json:"slots"`
}

func savedPool(key domain.PoolKey) quietSavedPool {
	saved := quietSavedPool{Kind: key.Kind, Address: key.Address.Hex()}
	if key.PoolID != (domain.Hash{}) {
		saved.PoolID = key.PoolID.Hex()
	}
	if key.Hooks != (domain.Address{}) {
		saved.Hooks = key.Hooks.Hex()
	}
	return saved
}

func (s quietSavedPool) key() (domain.PoolKey, error) {
	key := domain.PoolKey{Kind: s.Kind}
	var err error
	if key.Address, err = domain.ParseAddress(s.Address); err != nil {
		return key, err
	}
	if s.PoolID != "" {
		if key.PoolID, err = domain.ParseHash(s.PoolID); err != nil {
			return key, err
		}
	}
	if s.Hooks != "" {
		if key.Hooks, err = domain.ParseAddress(s.Hooks); err != nil {
			return key, err
		}
	}
	return key, nil
}

func decimal(v *big.Int) string {
	if v == nil {
		return ""
	}
	return v.String()
}

func parseDecimal(text string) (*big.Int, error) {
	if text == "" {
		return nil, nil
	}
	v, ok := new(big.Int).SetString(text, 10)
	if !ok || v.Sign() < 0 || v.BitLen() > 256 {
		return nil, errors.New("saved pool state out of range")
	}
	return v, nil
}

func (s quietSavedEntry) entry() (domain.PoolKey, quietEntry, error) {
	key, err := s.Pool.key()
	if err != nil {
		return key, quietEntry{}, err
	}
	e := quietEntry{updated: s.Updated, block: s.Block, state: pool.State{Key: key, UpdatedBlock: s.Updated}}
	for _, into := range []struct {
		value **big.Int
		text  string
	}{{&e.state.SqrtPriceX96, s.Price}, {&e.state.Liquidity, s.Liquidity}, {&e.state.Reserve0, s.Reserve0}, {&e.state.Reserve1, s.Reserve1}} {
		if *into.value, err = parseDecimal(into.text); err != nil {
			return key, quietEntry{}, err
		}
	}
	if !comparable(e.state) || len(s.Slots) == 0 {
		return key, quietEntry{}, errors.New("saved condition without a state to compare")
	}
	for _, pair := range s.Slots {
		var condition execution.SlotCondition
		if condition.Slot, err = domain.ParseHash(pair[0]); err != nil {
			return key, quietEntry{}, err
		}
		if condition.Value, err = domain.ParseHash(pair[1]); err != nil {
			return key, quietEntry{}, err
		}
		e.slots = append(e.slots, condition)
	}
	return key, e, nil
}

// quietModel is a saved word model. Shape says which fields it carries: the
// price and liquidity words of a V3-family pool ("v3", with the oracle's
// fields), a V4 pool ("v4") or a Camelot V3 pool ("camelot", with both fees
// and the timepoint index), or a V2 pair's reserves word ("v2"). A build from
// before a shape was added ignores a file that holds a model of it, like a
// file of another format.
type quietModel struct {
	Address       string   `json:"address"`
	PoolID        string   `json:"pool_id,omitempty"`
	Shape         string   `json:"shape"`
	Slots         []string `json:"slots"`
	Price         string   `json:"price,omitempty"`
	Tick          int32    `json:"tick,omitempty"`
	Index         uint16   `json:"index,omitempty"`
	Cardinality   uint16   `json:"cardinality,omitempty"`
	NextCard      uint16   `json:"cardinality_next,omitempty"`
	Rest          string   `json:"rest,omitempty"`
	Liquidity     string   `json:"liquidity,omitempty"`
	LiquidityHigh string   `json:"liquidity_high,omitempty"`
	Last          uint64   `json:"last,omitempty"`
	Unsure        bool     `json:"unsure,omitempty"`
	Reserve0      string   `json:"reserve0,omitempty"`
	Reserve1      string   `json:"reserve1,omitempty"`
	Stamp         uint32   `json:"stamp,omitempty"`
	FeeZto        uint16   `json:"fee_zto,omitempty"`
	FeeOtz        uint16   `json:"fee_otz,omitempty"`
}

var shapeNames = map[wordShape]string{shapeV3: "v3", shapeV4: "v4", shapeV2: "v2", shapeCamelot: "camelot"}

func savedModel(key wordKey, m *wordModel) quietModel {
	saved := quietModel{Address: key.address.Hex(), Shape: shapeNames[m.shape]}
	if key.id != (domain.Hash{}) {
		saved.PoolID = key.id.Hex()
	}
	for _, slot := range m.slots {
		saved.Slots = append(saved.Slots, slot.Hex())
	}
	if m.shape == shapeV2 {
		saved.Reserve0, saved.Reserve1, saved.Stamp = m.reserve0.String(), m.reserve1.String(), m.stamp
		return saved
	}
	saved.Price, saved.Tick, saved.Rest = m.price.String(), m.tick, m.rest.String()
	saved.Liquidity, saved.LiquidityHigh = m.liquidity.String(), m.liquidityHigh.String()
	saved.Index, saved.Cardinality, saved.NextCard, saved.Last, saved.Unsure = m.index, m.cardinality, m.nextCard, m.last, m.unsure
	saved.FeeZto, saved.FeeOtz = m.feeZto, m.feeOtz
	return saved
}

func quietStorePath(output string) string { return output + ".quiet.json" }

// save writes the models at the last applied block. It runs when the reader
// stops, after the last block was applied.
func (q *quietPools) save() {
	if q.store == "" {
		return
	}
	q.mu.Lock()
	if len(q.recent) == 0 {
		q.mu.Unlock()
		return
	}
	last := q.recent[len(q.recent)-1]
	file := quietFile{Format: quietStoreFormat, Block: last.number, Timestamp: last.timestamp, Models: make([]quietModel, 0, len(q.models))}
	for key, m := range q.models {
		file.Models = append(file.Models, savedModel(key, m))
	}
	for key, e := range q.entries {
		saved := quietSavedEntry{Pool: savedPool(key), Updated: e.updated, Block: e.block,
			Price: decimal(e.state.SqrtPriceX96), Liquidity: decimal(e.state.Liquidity), Reserve0: decimal(e.state.Reserve0), Reserve1: decimal(e.state.Reserve1)}
		for _, condition := range e.slots {
			saved.Slots = append(saved.Slots, [2]string{condition.Slot.Hex(), condition.Value.Hex()})
		}
		file.Entries = append(file.Entries, saved)
	}
	for key, layout := range q.layouts {
		saved := quietSavedLayout{Pool: savedPool(key)}
		for _, slot := range layout {
			saved.Slots = append(saved.Slots, slot.Hex())
		}
		file.Layouts = append(file.Layouts, saved)
	}
	q.mu.Unlock()
	raw, err := json.Marshal(file)
	if err == nil {
		err = writeFileAtomic(q.store, raw)
	}
	if err != nil {
		slog.Warn("pool word models not saved", "path", q.store, "error", err)
	}
}

// load restores saved models and the block they stand at. A missing file is
// a first start; an unreadable one only costs the models.
func (q *quietPools) load() {
	if q.store == "" {
		return
	}
	raw, err := os.ReadFile(q.store)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var file quietFile
	if err == nil {
		err = json.Unmarshal(raw, &file)
	}
	if err == nil && file.Format != quietStoreFormat {
		err = errors.New("unknown format")
	}
	models := make(map[wordKey]*wordModel, len(file.Models))
	for _, saved := range file.Models {
		if err != nil {
			break
		}
		var key wordKey
		var m *wordModel
		if key, m, err = saved.model(); err == nil {
			models[key] = m
		}
	}
	entries := make(map[domain.PoolKey]quietEntry, len(file.Entries))
	for _, saved := range file.Entries {
		if err != nil {
			break
		}
		var key domain.PoolKey
		var e quietEntry
		if key, e, err = saved.entry(); err == nil {
			entries[key] = e
		}
	}
	layouts := make(map[domain.PoolKey][]domain.Hash, len(file.Layouts))
	for _, saved := range file.Layouts {
		if err != nil {
			break
		}
		var key domain.PoolKey
		if key, err = saved.Pool.key(); err != nil {
			break
		}
		for _, text := range saved.Slots {
			var slot domain.Hash
			if slot, err = domain.ParseHash(text); err != nil {
				break
			}
			layouts[key] = append(layouts[key], slot)
		}
	}
	if err != nil {
		slog.Warn("pool word models not restored", "path", q.store, "error", err)
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.entries, q.layouts = entries, layouts
	q.models = models
	q.recent = []quietBlock{{number: file.Block, timestamp: file.Timestamp}}
	q.warmed = len(models) != 0
	slog.Info("pool word models restored", "path", q.store, "models", len(models), "block", file.Block)
}

func (s quietModel) model() (wordKey, *wordModel, error) {
	var key wordKey
	var err error
	if key.address, err = domain.ParseAddress(s.Address); err != nil {
		return key, nil, err
	}
	if s.PoolID != "" {
		if key.id, err = domain.ParseHash(s.PoolID); err != nil {
			return key, nil, err
		}
	}
	m := &wordModel{tick: s.Tick, index: s.Index, cardinality: s.Cardinality, nextCard: s.NextCard, last: s.Last, unsure: s.Unsure, stamp: s.Stamp, feeZto: s.FeeZto, feeOtz: s.FeeOtz}
	for _, text := range s.Slots {
		slot, err := domain.ParseHash(text)
		if err != nil {
			return key, nil, err
		}
		m.slots = append(m.slots, slot)
	}
	type field struct {
		into **big.Int
		text string
		bits int
	}
	fields := []field{{&m.price, s.Price, 160}, {&m.rest, s.Rest, 24}, {&m.liquidity, s.Liquidity, 128}, {&m.liquidityHigh, s.LiquidityHigh, 128}}
	slots := 2
	switch s.Shape {
	case "v3":
		m.shape = shapeV3
		if m.cardinality == 0 {
			return key, nil, errors.New("word model without an observation")
		}
	case "v4":
		m.shape = shapeV4
		fields[1].bits = 48 // the protocol fee and the LP fee
	case "camelot":
		m.shape = shapeCamelot // rest is both community fees and the lock
	case "v2":
		m.shape, slots = shapeV2, 1
		fields = []field{{&m.reserve0, s.Reserve0, 112}, {&m.reserve1, s.Reserve1, 112}}
	default:
		return key, nil, errors.New("word model of an unknown shape")
	}
	if len(m.slots) != slots || (m.shape == shapeV4) != (s.PoolID != "") {
		return key, nil, errors.New("word model with the wrong slots for its shape")
	}
	for _, field := range fields {
		value, ok := new(big.Int).SetString(field.text, 10)
		if !ok || value.Sign() < 0 || value.BitLen() > field.bits {
			return key, nil, errors.New("word model field out of range")
		}
		*field.into = value
	}
	return key, m, nil
}

// ready reports whether routes can be held: the models were restored, or a
// warm pass has read what it could since they were last dropped. It turns
// true on its own after quietReadyWait.
func (q *quietPools) ready() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.warmed || time.Since(q.waitingSince) >= quietReadyWait
}
