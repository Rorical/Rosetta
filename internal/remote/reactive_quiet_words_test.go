package remote

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

// wordFixture is a pool's condition words over real Arbitrum One blocks, read
// with eth_getStorageAt and eth_getLogs from an archive node: the price and
// liquidity words of a V3-family pool, the words of a V2 pair or a V4 pool at
// the slots it names, or those of a Camelot V3 pool.
type wordFixture struct {
	Pool          string   `json:"pool"`
	Kind          int      `json:"kind"`
	PoolID        string   `json:"pool_id"`
	PriceSlot     uint64   `json:"price_slot"`
	LiquiditySlot uint64   `json:"liquidity_slot"`
	Slots         []string `json:"slots"`
	Start         wordFixtureBlock
	Blocks        []wordFixtureBlock
}

type wordFixtureBlock struct {
	Number        uint64   `json:"number"`
	Timestamp     uint64   `json:"timestamp"`
	PriceWord     string   `json:"price_word"`
	LiquidityWord string   `json:"liquidity_word"`
	Words         []string `json:"words"`
	Logs          []struct {
		Topics []string `json:"topics"`
		Data   string   `json:"data"`
	} `json:"logs"`
}

// The fixtures: pairs of the Uniswap V2 and Sushi factories
// (one with a mint, one with a burn) and hookless V4 pools.
var (
	pairFixtures = []string{"v2-uniswap-006a31cb.json", "v2-uniswap-011f31d2.json", "v2-sushi-905dfcd5.json", "v2-uniswap-mint-0c93a1d3.json", "v2-sushi-burn-24704aff.json"}
	v4Fixtures   = []string{"v4-eth-usdc-500.json", "v4-wbtc-usdc-500.json", "v4-eth-usdc-3000.json", "v4-eth-arb-3000.json"}
	v3Fixtures   = []string{"v3.json", "slipstream.json"}
)

func loadWordFixture(t *testing.T, name string) (wordFixture, domain.Address) {
	t.Helper()
	raw, err := os.ReadFile("testdata/quiet-words/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var f wordFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	address, err := domain.ParseAddress(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	return f, address
}

func parseHash(t *testing.T, text string) domain.Hash {
	t.Helper()
	hash, err := domain.ParseHash(text)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// key is the fixture's pool as the pool store names it.
func (f wordFixture) key(t *testing.T, address domain.Address) domain.PoolKey {
	t.Helper()
	key := domain.PoolKey{Kind: domain.PoolKind(f.Kind), Address: address}
	if f.PoolID != "" {
		key.PoolID = parseHash(t, f.PoolID)
	}
	return key
}

func (f wordFixture) shape(t *testing.T) wordShape {
	t.Helper()
	shape, ok := shapeOf(domain.PoolKind(f.Kind))
	if !ok {
		t.Fatal("fixture of a kind without a word model")
	}
	return shape
}

func (f wordFixture) slots(t *testing.T, b wordFixtureBlock) []execution.SlotCondition {
	t.Helper()
	if len(f.Slots) == 0 {
		return []execution.SlotCondition{{Slot: slotKey(f.PriceSlot), Value: parseHash(t, b.PriceWord)}, {Slot: slotKey(f.LiquiditySlot), Value: parseHash(t, b.LiquidityWord)}}
	}
	var slots []execution.SlotCondition
	for i, slot := range f.Slots {
		slots = append(slots, execution.SlotCondition{Slot: parseHash(t, slot), Value: parseHash(t, b.Words[i])})
	}
	return slots
}

// state is the pool state that the words say: what the pool store holds once
// it has applied the block.
func (f wordFixture) state(t *testing.T, address domain.Address, words []execution.SlotCondition, updated uint64) pool.State {
	t.Helper()
	state := pool.State{Key: f.key(t, address), UpdatedBlock: updated}
	first := new(big.Int).SetBytes(words[0].Value[:])
	if state.Key.Kind == domain.PoolKindV2 {
		state.Reserve0, state.Reserve1 = new(big.Int).And(first, mask112), new(big.Int).And(new(big.Int).Rsh(first, 112), mask112)
		return state
	}
	state.SqrtPriceX96, state.Liquidity = new(big.Int).And(first, mask160), new(big.Int).And(new(big.Int).SetBytes(words[1].Value[:]), mask128)
	return state
}

func (b wordFixtureBlock) logs(t *testing.T, address domain.Address) []ingest.Log {
	t.Helper()
	var logs []ingest.Log
	for _, l := range b.Logs {
		log := ingest.Log{Address: address}
		for _, topic := range l.Topics {
			log.Topics = append(log.Topics, parseHash(t, topic))
		}
		data, err := hex.DecodeString(strings.TrimPrefix(l.Data, "0x"))
		if err != nil {
			t.Fatal(err)
		}
		log.Data = data
		logs = append(logs, log)
	}
	return logs
}

// The words derived from a pool's events equal the chain's after every block,
// on a Pancake V3 pool (zero-amount burns and collects among its logs) and a
// Slipstream pool (a mint and burns in range).
func TestDerivedWordsMatchTheChain(t *testing.T) {
	for _, name := range []string{"v3.json", "slipstream.json"} {
		t.Run(name, func(t *testing.T) {
			f, address := loadWordFixture(t, name)
			m := newWordModel(shapeV3, f.slots(t, f.Start), f.Start.Timestamp)
			if m == nil {
				t.Fatal("no model from the start words")
			}
			compared, restarts, writes := 0, 0, 0
			for _, block := range f.Blocks {
				want := f.slots(t, block)
				followed, before := true, m.index
				for _, log := range block.logs(t, address) {
					if !m.apply(log, block.Timestamp) {
						followed = false
						break
					}
				}
				if !followed {
					// A write in the very second the words were read.
					restarts++
					if m = newWordModel(shapeV3, want, block.Timestamp); m == nil {
						t.Fatal("no model from a block's words")
					}
					continue
				}
				if m.index != before {
					writes++
				}
				got := m.conditions()
				if got[0] != want[0] || got[1] != want[1] {
					t.Fatalf("block %d: derived %x %x, chain %x %x", block.Number, got[0].Value, got[1].Value, want[0].Value, want[1].Value)
				}
				compared++
			}
			if compared < 50 || restarts > 1 || writes == 0 {
				t.Fatalf("compared %d blocks, %d restarts, %d oracle writes", compared, restarts, writes)
			}
		})
	}
}

// The reader adopts words read a few blocks back, follows every applied block
// and, once the pool has rested, holds it without another read. A gap in the
// blocks drops the
// model: its events were not seen.
func TestQuietPoolsDeriveWordsFromAppliedBlocks(t *testing.T) {
	f, address := loadWordFixture(t, "v3.json")
	head := f.Start.Number
	q := newQuietPools(nil, func() uint64 { return head })
	var reasons []string
	q.note = func(reason string) { reasons = append(reasons, reason) }
	byNumber := map[uint64]wordFixtureBlock{}
	for _, block := range f.Blocks[:20] {
		byNumber[block.Number] = block
	}
	last := f.Blocks[19]
	timestamp := f.Start.Timestamp
	q.observeBlock(domain.BlockRef{Number: f.Start.Number, Timestamp: timestamp}, nil)
	for number := f.Start.Number + 1; number <= last.Number; number++ {
		var transactions []ingest.Transaction
		if block, ok := byNumber[number]; ok {
			timestamp = block.Timestamp
			transactions = []ingest.Transaction{{Successful: true, Logs: block.logs(t, address)}}
		}
		q.observeBlock(domain.BlockRef{Number: number, Timestamp: timestamp}, transactions)
		if number == f.Start.Number+3 {
			// The read of the start block returns three blocks later.
			q.mu.Lock()
			adopted := q.adopt(f.key(t, address), f.slots(t, f.Start), f.Start.Number)
			q.mu.Unlock()
			if !adopted {
				t.Fatal("words read inside the ring were not adopted")
			}
		}
	}
	want := f.slots(t, last)
	state := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: address}, UpdatedBlock: last.Number,
		SqrtPriceX96: new(big.Int).And(new(big.Int).SetBytes(want[0].Value[:]), mask160), Liquidity: new(big.Int).And(new(big.Int).SetBytes(want[1].Value[:]), mask128)}
	// The model has followed every applied block, but a pool that changed
	// within quietBlocks carries no condition.
	head = last.Number
	if q.conditions(state) != nil {
		t.Fatal("a pool that just changed was held")
	}
	head = last.Number + quietBlocks
	got := q.conditions(state)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("derived conditions %v, chain %v (%v)", got, want, reasons)
	}
	// A state the model does not describe is not held by it.
	moved := state.Copy()
	moved.SqrtPriceX96.Add(moved.SqrtPriceX96, big.NewInt(1))
	if q.conditions(moved) != nil {
		t.Fatal("a state the model does not describe was held")
	}
	q.mu.Lock()
	adopted := q.adopt(f.key(t, address), want, last.Number)
	q.mu.Unlock()
	if !adopted || q.conditions(state) == nil {
		t.Fatal("model not restored from the last block's words")
	}
	q.observeBlock(domain.BlockRef{Number: last.Number + 2, Timestamp: timestamp}, nil)
	if q.conditions(state) != nil {
		t.Fatal("a model that skipped a block still holds the pool")
	}
}

// Warming reads pools of the derived kinds before any route needs them, a few
// per call, and leaves pools that already have a model or whose words are
// only read.
func TestQuietPoolsWarmUnreadPools(t *testing.T) {
	q := newQuietPools(nil, func() uint64 { return 100 })
	var pools []pool.State
	for i := range 10 {
		pools = append(pools, pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{byte(i + 1)}}, SqrtPriceX96: big.NewInt(5), Liquidity: big.NewInt(7), UpdatedBlock: 50})
	}
	pools[0].UpdatedBlock = 95                      // changed within quietBlocks: read against its last Swap
	pools[1].Key.Kind = domain.PoolKindSolidlyV3    // not a derived kind
	q.models[modelKey(pools[2].Key)] = &wordModel{} // already followed
	q.warm(pools)
	if len(q.requests) != quietWarmReads {
		t.Fatalf("queued %d reads, want %d", len(q.requests), quietWarmReads)
	}
	for range quietWarmReads {
		state := <-q.requests
		if key := state.Key; key == pools[1].Key || key == pools[2].Key {
			t.Fatal("warmed a pool that needs no read", key.Address.Hex())
		}
	}
	// The next call continues where the last one stopped and skips the queued.
	if q.warmDue(time.Now()) || !q.warmDue(time.Now().Add(quietWarmEvery)) {
		t.Fatal("the next pass is due a second after one that queued reads")
	}
	q.warm(pools)
	if len(q.requests) != 4 {
		t.Fatalf("second pass queued %d reads, want the 4 pools left", len(q.requests))
	}
	q.warm(pools)
	if q.warmDue(time.Now().Add(quietWarmEvery)) || !q.warmDue(time.Now().Add(quietWarmIdle)) {
		t.Fatal("a pass with nothing to read must wait the idle interval")
	}
}

// The busiest pools, the ones rival arbitrage runs through most, trade every
// few blocks. Requiring a pool unchanged for quietBlocks before a first read
// would leave such a pool without a word model after a restart. Its state at
// the read block follows from its last Swap before that block, so the words are
// checked against that and the model is replayed to the head.
func TestQuietPoolsReadAPoolThatNeverRests(t *testing.T) {
	for _, name := range []string{"v3.json", "slipstream.json"} {
		f, address := loadWordFixture(t, name)
		adopted := 0
		for i := 1; i+1 < len(f.Blocks); i++ {
			at := f.Blocks[i]
			swap := false
			for _, log := range at.logs(t, address) {
				swap = log.Topics[0] == topicSwap || log.Topics[0] == topicSwapPancake
			}
			if !swap {
				continue // the block's last event must say the price and liquidity
			}
			head := at.Number + quietReadLag
			words := f.slots(t, at)
			server := &storageServer{words: map[domain.Hash]domain.Hash{words[0].Slot: words[0].Value, words[1].Slot: words[1].Value, slotKey(2): word(1, 1)}, blocks: map[string]bool{}}
			q := newQuietPools(server.serve(t), func() uint64 { return head })
			timestamp, latest := f.Start.Timestamp, f.Start
			byNumber := map[uint64]wordFixtureBlock{}
			for _, block := range f.Blocks {
				byNumber[block.Number] = block
			}
			for number := at.Number - 8; number <= head; number++ {
				var transactions []ingest.Transaction
				if block, ok := byNumber[number]; ok {
					timestamp, latest = block.Timestamp, block
					transactions = []ingest.Transaction{{Successful: true, Logs: block.logs(t, address)}}
				} else if number >= at.Number && timestamp < at.Timestamp {
					timestamp = at.Timestamp
				}
				q.observeBlock(domain.BlockRef{Number: number, Timestamp: timestamp}, transactions)
			}
			want := f.slots(t, latest)
			state := pool.State{Key: domain.PoolKey{Kind: domain.PoolKind(f.Kind), Address: address}, UpdatedBlock: head,
				SqrtPriceX96: new(big.Int).And(new(big.Int).SetBytes(want[0].Value[:]), mask160), Liquidity: new(big.Int).And(new(big.Int).SetBytes(want[1].Value[:]), mask128)}
			entry, layout, _, err := q.fetch(t.Context(), state)
			if err != nil {
				t.Fatalf("%s block %d: a pool changed at the head was not read: %v", name, at.Number, err)
			}
			if len(layout) != 2 || layout[0] != words[0].Slot || layout[1] != words[1].Slot {
				t.Fatalf("%s block %d: layout %v", name, at.Number, layout)
			}
			q.mu.Lock()
			q.adoptRead(state.Key, entry)
			q.mu.Unlock()
			// The model is at the head; it holds the pool once the pool has rested.
			head += quietBlocks
			got := q.conditions(state)
			if got == nil {
				continue // a write inside the read block's second cannot be told
			}
			if got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("%s block %d: conditions %v, chain %v", name, at.Number, got, want)
			}
			adopted++
		}
		t.Logf("%s: %d busy reads held at the chain's words", name, adopted)
		if adopted < 10 {
			t.Fatalf("%s: only %d busy reads adopted", name, adopted)
		}
	}
}

// Without a Swap of the pool in the ring before the read block there is
// nothing to check the words against, and the read waits.
func TestQuietPoolsDoNotReadABusyPoolWithoutAPriorSwap(t *testing.T) {
	f, address := loadWordFixture(t, "v3.json")
	head := f.Start.Number + 20
	q := newQuietPools(nil, func() uint64 { return head })
	for number := f.Start.Number; number <= head; number++ {
		q.observeBlock(domain.BlockRef{Number: number, Timestamp: f.Start.Timestamp}, nil)
	}
	state := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: address}, UpdatedBlock: head, SqrtPriceX96: big.NewInt(5000), Liquidity: big.NewInt(300)}
	if _, _, wait, err := q.fetch(t.Context(), state); err == nil || wait != quietRetry {
		t.Fatal("read without a state to compare with", err)
	}
}
