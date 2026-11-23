package remote

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// storageServer answers eth_getStorageAt batches from words and counts reads.
type storageServer struct {
	mu     sync.Mutex
	words  map[domain.Hash]domain.Hash
	reads  int
	blocks map[string]bool
}

func (s *storageServer) serve(t *testing.T) *rpc.Client {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var calls []struct {
			ID     uint64
			Method string
			Params []string
		}
		if err := json.NewDecoder(r.Body).Decode(&calls); err != nil {
			t.Error("storage reads were not batched", err)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var rows []any
		for _, call := range calls {
			slot, _ := domain.ParseHash(call.Params[1])
			s.reads++
			s.blocks[call.Params[2]] = true
			rows = append(rows, map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": s.words[slot].Hex()})
		}
		json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(server.Close)
	return rpc.NewClient(server.URL, rpc.Options{Timeout: time.Second})
}

func (s *storageServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// word puts value in the low bits under unrelated high bits, as slot0 packs
// the tick and oracle fields above the price.
func word(high, value int64) domain.Hash {
	var w domain.Hash
	new(big.Int).Add(new(big.Int).Lsh(big.NewInt(high), 200), big.NewInt(value)).FillBytes(w[:])
	return w
}

// waitHeld polls conditions while the reader works through its queue.
func waitHeld(t *testing.T, q *quietPools, state pool.State) []execution.SlotCondition {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if slots := q.conditions(state); slots != nil {
			return slots
		}
	}
	t.Fatal("quiet pool never held")
	return nil
}

func TestQuietPoolsDiscoverTheLayoutAndHoldTheState(t *testing.T) {
	// Slipstream's layout: price in slot 6, liquidity in slot 13.
	server := &storageServer{words: map[domain.Hash]domain.Hash{slotKey(6): word(7, 5000), slotKey(13): word(9, 300), slotKey(2): word(1, 1)}, blocks: map[string]bool{}}
	head := uint64(100)
	q := newQuietPools(server.serve(t), func() uint64 { return head })
	go q.run(t.Context())
	state := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{3}}, SqrtPriceX96: big.NewInt(5000), Liquidity: big.NewInt(300), UpdatedBlock: 80}

	slots := waitHeld(t, q, state)
	want := []execution.SlotCondition{{Slot: slotKey(6), Value: word(7, 5000)}, {Slot: slotKey(13), Value: word(9, 300)}}
	if !reflect.DeepEqual(slots, want) || server.count() != quietSlotRange || !server.blocks["0x60"] {
		t.Fatalf("slots %v after %d reads at %v", slots, server.count(), server.blocks)
	}

	// Within quietBlocks of the head a pool keeps its price bound, unread.
	recent := state
	recent.UpdatedBlock = 95
	if q.conditions(recent) != nil || server.count() != quietSlotRange {
		t.Fatal("a pool that just changed was held or read")
	}

	// A later version is read again, at the known slots only.
	server.mu.Lock()
	server.words[slotKey(6)] = word(7, 5100)
	server.mu.Unlock()
	head = 130
	moved := state
	moved.UpdatedBlock, moved.SqrtPriceX96 = 110, big.NewInt(5100)
	if q.conditions(moved) != nil {
		t.Fatal("words of the old version held the new one")
	}
	if slots := waitHeld(t, q, moved); slots[0].Value != word(7, 5100) || server.count() != quietSlotRange+2 {
		t.Fatal("known layout read again in full", server.count())
	}
}

// Words that disagree with our state are never held: our view or the chain
// moved, and the route keeps its price bound.
func TestQuietPoolsRejectStorageThatDiffersFromTheState(t *testing.T) {
	server := &storageServer{words: map[domain.Hash]domain.Hash{slotKey(0): word(7, 4999), slotKey(4): word(0, 300)}, blocks: map[string]bool{}}
	q := newQuietPools(server.serve(t), func() uint64 { return 100 })
	state := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{3}}, SqrtPriceX96: big.NewInt(5000), Liquidity: big.NewInt(300), UpdatedBlock: 80}
	if _, _, wait, err := q.fetch(t.Context(), state); err == nil || wait != quietUnsupported {
		t.Fatal("a price in no slot passed discovery", err)
	}
	q.layouts[state.Key] = []domain.Hash{slotKey(0), slotKey(4)}
	if _, _, wait, err := q.fetch(t.Context(), state); err == nil || wait != quietRetry {
		t.Fatal("a differing price was held", err)
	}
}

func TestDiscoverLayoutNeedsOneSlotEach(t *testing.T) {
	state := pool.State{SqrtPriceX96: big.NewInt(5000), Liquidity: big.NewInt(300)}
	words := make([]domain.Hash, quietSlotRange)
	words[2], words[3] = word(1, 5000), word(2, 300)
	if at, err := discoverLayout(words, state); err != nil || !slices.Equal(at, []int{2, 3}) {
		t.Fatal(at, err)
	}
	words[9] = word(3, 5000)
	if _, err := discoverLayout(words, state); err == nil {
		t.Fatal("two price slots accepted")
	}
	state.Liquidity = new(big.Int)
	if _, err := discoverLayout(make([]domain.Hash, quietSlotRange), state); err == nil {
		t.Fatal("zero liquidity matched empty slots")
	}
}

// A V2 pair's reserves share one word; a V4 pool's slot0 and liquidity sit at
// the PoolManager's computed slots (both checked on Arbitrum One).
func TestQuietPoolsHoldV2ReservesAndV4ManagerSlots(t *testing.T) {
	reserves := new(big.Int).Lsh(big.NewInt(7323762198), 112)
	reserves.Add(reserves, big.NewInt(41810613)).Add(reserves, new(big.Int).Lsh(big.NewInt(1794269756), 224))
	var packed domain.Hash
	reserves.FillBytes(packed[:])
	id := domain.Hash{0x02, 0xc0}
	v4 := v4Layout(id)
	server := &storageServer{words: map[domain.Hash]domain.Hash{slotKey(8): packed, v4[0]: word(5, 9000), v4[1]: word(0, 700)}, blocks: map[string]bool{}}
	q := newQuietPools(server.serve(t), func() uint64 { return 100 })
	go q.run(t.Context())
	pair := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{1}}, Reserve0: big.NewInt(41810613), Reserve1: big.NewInt(7323762198), UpdatedBlock: 50}
	if slots := waitHeld(t, q, pair); len(slots) != 1 || slots[0] != (execution.SlotCondition{Slot: slotKey(8), Value: packed}) {
		t.Fatal("V2 reserves", slots)
	}
	manager := domain.Address{0x36}
	v4Pool := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV4, Address: manager, PoolID: id}, SqrtPriceX96: big.NewInt(9000), Liquidity: big.NewInt(700), UpdatedBlock: 50}
	before := server.count()
	if slots := waitHeld(t, q, v4Pool); len(slots) != 2 || slots[0].Slot != v4[0] || slots[1].Slot != v4[1] || server.count() != before+2 {
		t.Fatal("V4 slots", slots, server.count()-before)
	}
	// The slot is keccak256(poolId, 6), the mapping PriceCheck.v4Price reads.
	want := crypto.Keccak256Hash(id[:], common.LeftPadBytes([]byte{6}, 32))
	if v4[0] != domain.Hash(want) {
		t.Fatal("V4 state slot", v4[0].Hex(), want.Hex())
	}
}

// The reactive context is refreshed a few times a minute; as the
// quiet reader's head it would make a pool look recently changed for up to
// that long, read its storage at a block the pool had moved past, and leave
// most long-quiet legs without a condition.
func TestQuietHeadFollowsAppliedBlocksNotTheContext(t *testing.T) {
	s := &reactiveRuntime{}
	s.cache.Store(&reactiveCache{block: domain.BlockRef{Number: 100}})
	if s.quietHead() != 100 {
		t.Fatal("no applied block yet: the context block stands in")
	}
	s.head.Store(220)
	if s.quietHead() != 220 {
		t.Fatal("quiet head is the stale context block", s.quietHead())
	}
	s.cache.Store(nil)
	if s.quietHead() != 0 {
		t.Fatal("a failed refresh must stop conditions")
	}
}

// The Arbitrum One sequencer admits a condition only if it also held on a
// state at least 2 s old. Sends it rejects with "Storage slot value condition
// not met" hold a pool that changed inside that window, even when every
// condition stays true on chain for the next blocks. A pool that
// changed within quietBlocks, or in a block still pending, carries no
// condition even when its derived words are right.
func TestAPoolThatChangedWithinTheAdmissionWindowIsNotHeld(t *testing.T) {
	key := domain.PoolKey{Kind: domain.PoolKindV3, Address: addressAt(7)}
	state := pool.State{Key: key, Token0: addressAt(1), Token1: addressAt(2), Fee: 500, TickSpacing: 10, SqrtPriceX96: big.NewInt(1 << 40), Liquidity: big.NewInt(5), UpdatedBlock: 100}
	held := []execution.SlotCondition{{Value: domain.Hash{1}}}
	head := uint64(100)
	q := newQuietPools(nil, func() uint64 { return head })
	q.entries[key] = quietEntry{updated: 100, state: state.Copy(), slots: held}
	reasons := map[string]int{}
	q.note = func(reason string) { reasons[reason]++ }

	for head = 100; head < 100+quietBlocks; head++ {
		if slots := q.conditions(state); slots != nil {
			t.Fatalf("held %d blocks after its last change: %+v", head-100, slots)
		}
	}
	if slots := q.conditions(state); len(slots) != 1 {
		t.Fatalf("not held %d blocks after its last change (%v)", head-100, reasons)
	}
	// A log of a block that is not applied yet: the pool is moving again.
	q.touched[modelKey(key)] = head + 1
	if slots := q.conditions(state); slots != nil {
		t.Fatalf("held with a pending change: %+v", slots)
	}
}
