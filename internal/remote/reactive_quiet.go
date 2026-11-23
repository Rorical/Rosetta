package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// quietBlocks is how long a pool must stay unchanged before a route holds it
// by condition: Nitro admits a condition only if it also held on a state at
// least 2 s old, and 12 blocks (3 s) leave margin for whole-second
// block timestamps.
const quietBlocks = 12

// quietCheckEvery spaces the comparisons of a pool's derived words with a
// read of the chain.
const quietCheckEvery = 10 * time.Minute

// quietReadLag is how far behind the head the words are read: the provider
// may not serve the newest feed block yet, and a pool quiet for quietBlocks
// has the same words there.
const quietReadLag = 4

// quietSlotRange is how many leading storage slots layout discovery reads. On
// Arbitrum One the price and liquidity words sit in slots 0 and 4
// (Uniswap, Sushi), 0 and 5 (Pancake), 6 and 13 (Slipstream), 2 and 3
// (Camelot), and a V2 pair's reserves in slot 8.
const quietSlotRange = 16

// v4PoolsSlot is PoolManager's mapping from PoolId to Pool.State; a pool's
// slot0 is the state's first word and its liquidity the fourth.
const v4PoolsSlot = 6

// quietRetry spaces reads of a pool whose words did not match our state;
// quietUnsupported spaces discoveries that found no single matching slot.
const (
	quietRetry       = 10 * time.Second
	quietUnsupported = 10 * time.Minute
)

// quietPools holds the storage words of pools for route conditions: price and liquidity for V3-family, Camelot V3 and V4 pools, the
// reserves word for V2 pairs. The words are read off the send path at a context block and kept
// only while they match our state and its last update; for the kinds with a
// word model (derivedKind) a read starts the model, which then follows the
// pool's events. V3-family and
// V2 layouts are found by matching that state against the leading slots,
// since forks lay them out differently and Pancake shares Uniswap's pool
// kind; V4 pools sit at computed slots of the PoolManager. A rival that moves
// a held pool first voids the transaction free.
type quietPools struct {
	read *rpc.Client
	// head is the latest applied canonical block; the words are read
	// quietReadLag behind it.
	head     func() uint64
	requests chan pool.State
	mu       sync.Mutex
	layouts  map[domain.PoolKey][]domain.Hash
	entries  map[domain.PoolKey]quietEntry
	queued   map[domain.PoolKey]bool
	retry    map[domain.PoolKey]time.Time
	// models derive the words of V3-family, Camelot V3, V4 and V2 pools from
	// their events (reactive_quiet_words.go); recent holds the last applied
	// blocks. unfit are pairs, V4 pools and Camelot pools whose words turned
	// out not to have their kind's shape: they are held by reads only.
	models map[wordKey]*wordModel
	unfit  map[wordKey]bool
	recent []quietBlock
	// touched names the pools with a log in the Fast view's blocks past the
	// last applied one, with the last such block.
	touched map[wordKey]uint64
	// store keeps the models across restarts (reactive_quiet_store.go).
	// warmed says a warm pass found nothing left to read since the models
	// were last dropped or restored; until then, or quietReadyWait after
	// waitingSince, sends wait (ready).
	store        string
	warmed       bool
	waitingSince time.Time
	warmNext     int       // round-robin position of warm
	warmAt       time.Time // when the next warm call is due
	// checked is when a pool's derived words were last compared with a read.
	checked map[domain.PoolKey]time.Time
	// note, when set, counts why a pool was or was not held and why a read
	// failed (the reactive skip counters): without it a route without
	// conditions says nothing about the cause.
	note func(string)
}

func (q *quietPools) count(reason string) {
	if q.note != nil {
		q.note(reason)
	}
}

// quietEntry holds a pool's words for the state they were checked against.
type quietEntry struct {
	updated uint64
	state   pool.State
	slots   []execution.SlotCondition
	block   uint64 // where the words were read
}

func newQuietPools(read *rpc.Client, head func() uint64) *quietPools {
	return &quietPools{read: read, head: head, requests: make(chan pool.State, 64),
		layouts: map[domain.PoolKey][]domain.Hash{}, entries: map[domain.PoolKey]quietEntry{}, queued: map[domain.PoolKey]bool{}, retry: map[domain.PoolKey]time.Time{},
		models: map[wordKey]*wordModel{}, unfit: map[wordKey]bool{}, touched: map[wordKey]uint64{}, checked: map[domain.PoolKey]time.Time{}, waitingSince: time.Now()}
}

// quietKind reports whether conditions can hold pools of kind.
func quietKind(kind domain.PoolKind) bool {
	switch kind {
	case domain.PoolKindV2, domain.PoolKindV3, domain.PoolKindSlipstream, domain.PoolKindCamelotV3, domain.PoolKindSolidlyV3, domain.PoolKindV4:
		return true
	}
	return false
}

// conditions returns the slot conditions that hold the pool of state, or nil
// when it changed within quietBlocks of the head, its kind is not held, or its
// words for this state are not read yet. A miss queues a read for the next
// route: once per pool state, and again after quietRetry when the words did
// not match. There is no cap per minute: the reads serve routes that
// are being priced, and a held pool turns a lost race from a paid revert into
// a free drop.
func (q *quietPools) conditions(state pool.State) []execution.SlotCondition {
	if !quietKind(state.Key.Kind) || !comparable(state) {
		return nil
	}
	recent := q.head() < state.UpdatedBlock+quietBlocks
	key := state.Key
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	derives, model := q.derives(key), modelKey(key)
	// A change in a block that is not applied yet counts as well: the
	// sequencer checks the condition on a state at least 2 s old, where
	// this pool still had its earlier words.
	if _, pending := q.touched[model]; recent || pending {
		// A read lags the head; a pool that just changed has moved past it.
		// A derived kind is read all the same: its model is replayed to the
		// head and holds the pool once it has rested.
		q.count("quiet_pool_recent")
		if derives && len(q.recent) != 0 && q.models[model] == nil && !q.queued[key] && !now.Before(q.retry[key]) {
			select {
			case q.requests <- state.Copy():
				q.queued[key] = true
				q.count("quiet_pool_read_queued")
			default:
			}
		}
		return nil
	}
	if m := q.models[model]; m != nil && derives {
		if m.matches(state) {
			q.count("quiet_pool_derived")
			// Compare with the chain now and then: a wrong word costs nothing
			// but drops every send that holds it.
			if now.Sub(q.checked[key]) >= quietCheckEvery && !q.queued[key] {
				select {
				case q.requests <- state.Copy():
					q.queued[key], q.checked[key] = true, now
				default:
				}
			}
			return m.conditions()
		}
		delete(q.models, model)
		q.count("quiet_words_differ_from_state")
	}
	if e, ok := q.entries[key]; ok && e.updated == state.UpdatedBlock && sameState(e.state, state) {
		q.count("quiet_pool_held")
		return e.slots
	}
	if q.queued[key] || now.Before(q.retry[key]) {
		q.count("quiet_pool_waiting")
		return nil
	}
	select {
	case q.requests <- state.Copy():
		q.queued[key] = true
		q.count("quiet_pool_read_queued")
	default:
		q.count("quiet_pool_queue_full")
	}
	return nil
}

func (q *quietPools) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			q.save()
			return
		case state := <-q.requests:
			entry, layout, wait, err := q.fetch(ctx, state)
			q.mu.Lock()
			delete(q.queued, state.Key)
			if err != nil {
				q.retry[state.Key] = time.Now().Add(wait)
				q.count("quiet_read_" + quietFailure(err))
			} else {
				q.count("quiet_read_ok")
				q.layouts[state.Key], q.entries[state.Key] = layout, entry
				q.adoptRead(state.Key, entry)
			}
			q.mu.Unlock()
		}
	}
}

// fetch reads the pool's words at the head and checks them against state; on
// failure it returns how long to wait before reading the pool again.
func (q *quietPools) fetch(ctx context.Context, state pool.State) (quietEntry, []domain.Hash, time.Duration, error) {
	head := q.head()
	block := head - quietReadLag
	if head < state.UpdatedBlock+quietBlocks {
		// A pool that changed since the read block cannot be compared with
		// our state. Its last Swap up to that block says what it was.
		at, ok := q.swapState(state, block)
		if !ok {
			return quietEntry{}, nil, quietRetry, fmt.Errorf("head within quietBlocks of the pool's state")
		}
		state = at
	}
	q.mu.Lock()
	layout, known := q.layouts[state.Key]
	q.mu.Unlock()
	if !known && state.Key.IsV4() {
		layout, known = v4Layout(state.Key.PoolID), true
	}
	slots := layout
	if !known {
		slots = make([]domain.Hash, quietSlotRange)
		for i := range slots {
			slots[i] = slotKey(uint64(i))
		}
	}
	words, err := q.storage(ctx, state.Key.Address, slots, block)
	if err != nil {
		return quietEntry{}, nil, quietRetry, err
	}
	if !known {
		at, err := discoverLayout(words, state)
		if err != nil {
			return quietEntry{}, nil, quietUnsupported, err
		}
		layout = make([]domain.Hash, len(at))
		picked := make([]domain.Hash, len(at))
		for i, index := range at {
			layout[i], picked[i] = slots[index], words[index]
		}
		words = picked
	}
	if !holdsState(words, state) {
		return quietEntry{}, nil, quietRetry, fmt.Errorf("pool storage differs from our state")
	}
	entry := quietEntry{updated: state.UpdatedBlock, state: state, block: block}
	for i, slot := range layout {
		entry.slots = append(entry.slots, execution.SlotCondition{Slot: slot, Value: words[i]})
	}
	return entry, layout, 0, nil
}

// v4Layout is a V4 pool's slot0 and liquidity slots in the PoolManager.
func v4Layout(id domain.Hash) []domain.Hash {
	base := slotKey(v4PoolsSlot)
	copy(base[:], crypto.Keccak256(id[:], base[:]))
	var liquidity domain.Hash
	new(big.Int).Add(new(big.Int).SetBytes(base[:]), big.NewInt(3)).FillBytes(liquidity[:])
	return []domain.Hash{base, liquidity}
}

// discoverLayout finds the words that hold state among the leading slots:
// for a V2 pair the one slot packing both reserves, otherwise the one slot
// whose low 160 bits are the price and the one other whose low 128 bits are
// the liquidity. It returns their indices in holdsState's order.
func discoverLayout(words []domain.Hash, state pool.State) ([]int, error) {
	if state.Key.Kind == domain.PoolKindV2 {
		reserves := matching(words, -1, func(w domain.Hash) bool { return holdsReserves(w, state.Reserve0, state.Reserve1) })
		if len(reserves) != 1 {
			return nil, fmt.Errorf("no single reserves slot")
		}
		return reserves, nil
	}
	if state.Liquidity.Sign() == 0 {
		return nil, fmt.Errorf("no liquidity to match")
	}
	price := matching(words, -1, func(w domain.Hash) bool { return holdsPrice(w, state.SqrtPriceX96) })
	if len(price) != 1 {
		return nil, fmt.Errorf("no single price slot")
	}
	liquidity := matching(words, price[0], func(w domain.Hash) bool { return holdsLiquidity(w, state.Liquidity) })
	if len(liquidity) != 1 {
		return nil, fmt.Errorf("no single liquidity slot")
	}
	return []int{price[0], liquidity[0]}, nil
}

func matching(words []domain.Hash, skip int, match func(domain.Hash) bool) []int {
	var found []int
	for i, word := range words {
		if i != skip && match(word) {
			found = append(found, i)
		}
	}
	return found
}

// comparable reports whether state carries the values its words are checked
// against.
func comparable(state pool.State) bool {
	if state.Key.Kind == domain.PoolKindV2 {
		return state.Reserve0 != nil && state.Reserve1 != nil
	}
	return state.SqrtPriceX96 != nil && state.Liquidity != nil
}

func holdsState(words []domain.Hash, state pool.State) bool {
	if state.Key.Kind == domain.PoolKindV2 {
		return len(words) == 1 && holdsReserves(words[0], state.Reserve0, state.Reserve1)
	}
	return len(words) == 2 && holdsPrice(words[0], state.SqrtPriceX96) && holdsLiquidity(words[1], state.Liquidity)
}

func sameState(a, b pool.State) bool {
	if a.Key.Kind == domain.PoolKindV2 {
		return a.Reserve0.Cmp(b.Reserve0) == 0 && a.Reserve1.Cmp(b.Reserve1) == 0
	}
	return a.SqrtPriceX96.Cmp(b.SqrtPriceX96) == 0 && a.Liquidity.Cmp(b.Liquidity) == 0
}

func holdsPrice(word domain.Hash, price *big.Int) bool {
	return new(big.Int).SetBytes(word[12:]).Cmp(price) == 0
}

func holdsLiquidity(word domain.Hash, liquidity *big.Int) bool {
	return new(big.Int).SetBytes(word[16:]).Cmp(liquidity) == 0
}

// holdsReserves matches a Uniswap V2 pair's packed word: reserve0 in the low
// 112 bits, reserve1 in the next 112, the last sync's timestamp above.
func holdsReserves(word domain.Hash, reserve0, reserve1 *big.Int) bool {
	value := new(big.Int).SetBytes(word[:])
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))
	low := new(big.Int).And(value, mask)
	high := new(big.Int).And(new(big.Int).Rsh(value, 112), mask)
	return low.Cmp(reserve0) == 0 && high.Cmp(reserve1) == 0
}

func slotKey(slot uint64) domain.Hash {
	var key domain.Hash
	new(big.Int).SetUint64(slot).FillBytes(key[:])
	return key
}

// storage reads the slots at block in batches of eight, as batchStrings does.
func (q *quietPools) storage(ctx context.Context, address domain.Address, slots []domain.Hash, block uint64) ([]domain.Hash, error) {
	at := fmt.Sprintf("0x%x", block)
	words := make([]domain.Hash, 0, len(slots))
	for start := 0; start < len(slots); start += 8 {
		var calls []rpc.Call
		for _, slot := range slots[start:min(start+8, len(slots))] {
			calls = append(calls, rpc.Call{Method: "eth_getStorageAt", Params: []any{address.Hex(), slot.Hex(), at}})
		}
		results, err := q.read.Batch(ctx, calls)
		if err != nil {
			return nil, err
		}
		for _, result := range results {
			if result.Error != nil {
				return nil, result.Error
			}
			var text string
			if err := json.Unmarshal(result.Result, &text); err != nil {
				return nil, err
			}
			raw, err := hexData(text)
			if err != nil || len(raw) > 32 {
				return nil, fmt.Errorf("invalid storage word")
			}
			var word domain.Hash
			copy(word[32-len(raw):], raw)
			words = append(words, word)
		}
	}
	return words, nil
}

// quietFailure names a failed read for the counters. Only this file's own
// messages are spelled out: a provider error may quote the endpoint.
func quietFailure(err error) string {
	switch text := err.Error(); text {
	case "head within quietBlocks of the pool's state", "pool storage differs from our state", "no single reserves slot", "no liquidity to match", "no single price slot", "no single liquidity slot":
		return strings.ReplaceAll(text, " ", "_")
	}
	return "rpc_error"
}

// adoptRead uses a successful read of a derived kind: it checks a model that
// already follows the pool, or starts one. The caller holds mu.
func (q *quietPools) adoptRead(key domain.PoolKey, entry quietEntry) {
	if !q.derives(key) {
		return
	}
	model := modelKey(key)
	if m := q.models[model]; m != nil && m.matches(entry.state) {
		if slices.Equal(m.conditions(), entry.slots) {
			q.count("quiet_words_check_ok")
			return
		}
		q.count("quiet_words_check_mismatch")
		if key.Kind == domain.PoolKindV2 && !q.syncedAfter(model, entry.block) {
			// A pair's word follows from its Syncs with nothing left to
			// doubt, so a difference means the pair does not pack Uniswap's
			// word: it gets no model again. (A Sync after the read block is
			// the innocent case: the same reserves at a later time.)
			delete(q.models, model)
			q.unfit[model] = true
			slog.Warn("derived pair word differs from the chain; pair held by reads only", "pool", key.Address.Hex(), "block", entry.block)
			return
		}
		slog.Warn("derived pool words differ from the chain; model restarted", "pool", key.Address.Hex(), "block", entry.block)
	}
	if q.adopt(key, entry.slots, entry.block) {
		q.count("quiet_words_adopted")
	} else {
		delete(q.models, model)
		q.count("quiet_words_not_adopted")
	}
}
