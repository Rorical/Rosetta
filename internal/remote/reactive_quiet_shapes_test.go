package remote

import (
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Paid reverts cluster on legs without a condition. Words read a few blocks
// behind the head need the pool unchanged for quietBlocks, so the pool a
// route reacts to would never have one. V2 and V4 words follow
// from their events as the V3 words do: every block of every fixture must
// give the chain's words, with no restart.
func TestPairAndV4WordsMatchTheChain(t *testing.T) {
	type tally struct{ blocks, several, syncs, twoSyncs, noSync, mints, burns, swaps, inRange, outOfRange, zero int }
	var pairs, v4 tally
	for _, name := range slices.Concat(pairFixtures, v4Fixtures) {
		f, address := loadWordFixture(t, name)
		shape, total := f.shape(t), &pairs
		if shape == shapeV4 {
			total = &v4
			// The fixture's slots are keccak256(id, 6) and that plus 3,
			// computed by the script on its own.
			if layout := v4Layout(parseHash(t, f.PoolID)); f.Slots[0] != layout[0].Hex() || f.Slots[1] != layout[1].Hex() {
				t.Fatalf("%s: slots %v, layout %v", name, f.Slots, layout)
			}
		}
		m := newWordModel(shape, f.slots(t, f.Start), f.Start.Timestamp)
		if m == nil {
			t.Fatalf("%s: no model from the start words", name)
		}
		before := *total
		for _, block := range f.Blocks {
			syncs := 0
			for _, log := range block.logs(t, address) {
				switch log.Topics[0] {
				case topicSync:
					syncs++
				case topicHash("4c209b5fc8ad50758f13e2e1088ba56a560dff690a1c6fef26394f4c03821c4f"):
					total.mints++
				case topicHash("dccd412f0b1252819cb1fd330b93224ca42612892bb3f4f789976e6d81936496"):
					total.burns++
				case topicV4Swap:
					total.swaps++
				case topicV4Modify:
					delta, _ := dataWord(log.Data, 2)
					switch lower, upper := tick24(log.Data[0:32]), tick24(log.Data[32:64]); {
					case delta.Sign() == 0:
						total.zero++
					case m.tick >= lower && m.tick < upper:
						total.inRange++
					default:
						total.outOfRange++
					}
				}
				if !m.apply(log, block.Timestamp) {
					t.Fatalf("%s block %d: model dropped", name, block.Number)
				}
			}
			if got, want := m.conditions(), f.slots(t, block); !slices.Equal(got, want) {
				t.Fatalf("%s block %d: derived %x, chain %x", name, block.Number, got, want)
			}
			total.blocks++
			total.syncs += syncs
			if len(block.Logs) > 1 {
				total.several++
			}
			if shape == shapeV2 && syncs > 1 {
				total.twoSyncs++
			}
			if shape == shapeV2 && syncs == 0 {
				total.noSync++
			}
		}
		t.Logf("%s: %d blocks equal to the chain", name, total.blocks-before.blocks)
	}
	t.Logf("V2: %+v", pairs)
	t.Logf("V4: %+v", v4)
	if pairs.blocks < 200 || pairs.mints == 0 || pairs.burns == 0 || pairs.twoSyncs == 0 || pairs.noSync == 0 {
		t.Fatalf("V2 fixtures cover too little: %+v", pairs)
	}
	if v4.blocks < 300 || v4.inRange == 0 || v4.outOfRange == 0 || v4.several == 0 {
		t.Fatalf("V4 fixtures cover too little: %+v", v4)
	}
}

// Two V4 pools sit behind the one PoolManager address: each has its own model,
// follows only the logs that name its ID, and has its own words after every
// applied block.
func TestQuietPoolsFollowV4PoolsBehindOneManager(t *testing.T) {
	type followed struct {
		f        wordFixture
		key      domain.PoolKey
		byNumber map[uint64]wordFixtureBlock
		words    []execution.SlotCondition
		compared int
	}
	var pools []*followed
	var manager domain.Address
	first, last := ^uint64(0), uint64(0)
	for _, name := range []string{"v4-eth-usdc-500.json", "v4-eth-usdc-3000.json"} {
		f, address := loadWordFixture(t, name)
		p := &followed{f: f, key: f.key(t, address), byNumber: map[uint64]wordFixtureBlock{}}
		for _, block := range f.Blocks {
			p.byNumber[block.Number] = block
		}
		pools, manager = append(pools, p), address
		first, last = min(first, f.Start.Number), max(last, f.Blocks[len(f.Blocks)-1].Number)
	}
	if pools[0].key.Address != pools[1].key.Address || pools[0].key.PoolID == pools[1].key.PoolID {
		t.Fatal("the fixtures must be two pools of one PoolManager")
	}
	head := first
	q := newQuietPools(nil, func() uint64 { return head })
	timestamp := uint64(0)
	for head = first; head <= last; head++ {
		var transactions []ingest.Transaction
		for _, p := range pools {
			if block, ok := p.byNumber[head]; ok {
				timestamp = block.Timestamp
				transactions = append(transactions, ingest.Transaction{Successful: true, Logs: block.logs(t, manager)})
			}
		}
		q.observeBlock(domain.BlockRef{Number: head, Timestamp: timestamp}, transactions)
		for _, p := range pools {
			if head == p.f.Start.Number {
				p.words = p.f.slots(t, p.f.Start)
				q.mu.Lock()
				adopted := q.adopt(p.key, p.words, head)
				q.mu.Unlock()
				if !adopted {
					t.Fatal("start words not adopted")
				}
			}
			block, ok := p.byNumber[head]
			if !ok {
				continue
			}
			p.words = p.f.slots(t, block)
			m := q.models[modelKey(p.key)]
			if m == nil || !m.matches(p.f.state(t, manager, p.words, head)) || !slices.Equal(m.conditions(), p.words) {
				t.Fatalf("pool %s block %d: model not at the chain's words %x", p.key.PoolID.Hex(), head, p.words)
			}
			p.compared++
		}
	}
	if pools[0].compared < 60 || pools[1].compared < 60 || len(q.models) != 2 {
		t.Fatalf("compared %d and %d blocks with %d models", pools[0].compared, pools[1].compared, len(q.models))
	}
}

// A pool with a log in a pending block carries no condition, whatever its
// model says. Every block of every fixture is replayed as that pending
// block over a model at the block before: once the block is applied and the
// pool has rested, it is held at the chain's words. A V2 pair, a V4 pool and a
// Camelot pool are held after every block; a V3 pool is not held only when its
// model was read in the block's own second and so does not know whether the
// oracle has written.
func TestQuietPoolsHoldAPendingChangeOnlyOnceAppliedAndRested(t *testing.T) {
	for _, name := range slices.Concat(pairFixtures, v4Fixtures, v3Fixtures, camelotFixtures) {
		f, address := loadWordFixture(t, name)
		key, shape := f.key(t, address), f.shape(t)
		held, skipped := 0, 0
		previous := f.Start
		for i, block := range f.Blocks {
			if block.Number <= previous.Number+1 {
				previous = block
				continue
			}
			head := block.Number - 1
			q := newQuietPools(nil, func() uint64 { return head })
			// The last applied block's time is the pending block's own second or
			// the one before.
			applied := block.Timestamp
			if i%2 == 1 && previous.Timestamp < applied {
				applied--
			}
			q.observeBlock(domain.BlockRef{Number: head - 1, Timestamp: applied}, nil)
			q.observeBlock(domain.BlockRef{Number: head, Timestamp: applied}, nil)
			q.mu.Lock()
			adopted := q.adopt(key, f.slots(t, previous), head)
			sure := shape != shapeV3
			if m := q.models[modelKey(key)]; m != nil && shape == shapeV3 && previous.Timestamp < applied {
				// The fixture knows when the pool was last written.
				m.last, m.unsure, sure = previous.Timestamp, false, true
			}
			q.mu.Unlock()
			if !adopted {
				t.Fatalf("%s block %d: words not adopted", name, block.Number)
			}
			q.observeFast(block.Number, block.logs(t, address))
			// Even a state that matches the model and has rested is not held
			// while the pool has a pending log that can move its words.
			head = block.Number + quietBlocks
			moves := slices.ContainsFunc(block.logs(t, address), func(log ingest.Log) bool { return followedTopic(log.Topics[0]) })
			if got := q.conditions(f.state(t, address, f.slots(t, previous), previous.Number)); moves && got != nil {
				t.Fatalf("%s block %d: held at %x with a pending change", name, block.Number, got)
			}
			q.observeBlock(domain.BlockRef{Number: block.Number, Timestamp: block.Timestamp}, []ingest.Transaction{{Successful: true, Logs: block.logs(t, address)}})
			want := f.slots(t, block)
			state := f.state(t, address, want, block.Number)
			head = block.Number
			if got := q.conditions(state); got != nil {
				t.Fatalf("%s block %d: held at %x in the block of its change", name, block.Number, got)
			}
			head = block.Number + quietBlocks
			got := q.conditions(state)
			switch {
			case got == nil && !sure:
				skipped++
			case !slices.Equal(got, want):
				t.Fatalf("%s block %d: held at %x, chain %x", name, block.Number, got, want)
			default:
				held++
			}
			previous = block
		}
		t.Logf("%s: %d changes held at the chain's words once rested, %d not held", name, held, skipped)
		if shape != shapeV3 && skipped != 0 {
			t.Fatalf("%s: %d changes of an exact kind not held", name, skipped)
		}
	}
}

// The busy-read form for pairs, V4 pools and Camelot pools: a pool that
// changed within quietBlocks of the head is read against its last Sync or
// Swap up to the read block, and the model is replayed to the head.
func TestQuietPoolsReadPairsAndV4PoolsThatNeverRest(t *testing.T) {
	for _, name := range slices.Concat(pairFixtures, v4Fixtures, camelotFixtures) {
		f, address := loadWordFixture(t, name)
		key, shape := f.key(t, address), f.shape(t)
		byNumber := map[uint64]wordFixtureBlock{}
		for _, block := range f.Blocks {
			byNumber[block.Number] = block
		}
		adopted, unstated := 0, 0
		for i := 0; i < len(f.Blocks); i++ {
			at := f.Blocks[i]
			var lastFollowed domain.Hash
			for _, log := range at.logs(t, address) {
				if followedTopic(log.Topics[0]) {
					lastFollowed = log.Topics[0]
				}
			}
			if lastFollowed != topicSync && lastFollowed != topicV4Swap && (shape != shapeCamelot || lastFollowed != topicSwap) {
				unstated++
				continue // the block's last event must state the reserves, or the price and liquidity
			}
			head := at.Number + quietReadLag
			words := f.slots(t, at)
			server := &storageServer{words: map[domain.Hash]domain.Hash{slotKey(2): word(1, 1)}, blocks: map[string]bool{}}
			for _, condition := range words {
				server.words[condition.Slot] = condition.Value
			}
			q := newQuietPools(server.serve(t), func() uint64 { return head })
			timestamp, latest := f.Start.Timestamp, f.Start
			if i > 0 {
				timestamp, latest = f.Blocks[i-1].Timestamp, f.Blocks[i-1]
			}
			for number := at.Number - 8; number <= head; number++ {
				var transactions []ingest.Transaction
				if block, ok := byNumber[number]; ok {
					timestamp, latest = block.Timestamp, block
					transactions = []ingest.Transaction{{Successful: true, Logs: block.logs(t, address)}}
				}
				q.observeBlock(domain.BlockRef{Number: number, Timestamp: timestamp}, transactions)
			}
			want := f.slots(t, latest)
			state := f.state(t, address, want, head)
			entry, layout, _, err := q.fetch(t.Context(), state)
			if err != nil {
				t.Fatalf("%s block %d: a pool changed at the head was not read: %v", name, at.Number, err)
			}
			for j, slot := range layout {
				if len(layout) != len(words) || slot != words[j].Slot {
					t.Fatalf("%s block %d: layout %v", name, at.Number, layout)
				}
			}
			q.mu.Lock()
			q.adoptRead(state.Key, entry)
			q.mu.Unlock()
			// The model is at the head; it holds the pool once the pool has rested.
			head += quietBlocks
			if got := q.conditions(state); !slices.Equal(got, want) {
				t.Fatalf("%s block %d: conditions %x, chain %x", name, at.Number, got, want)
			}
			if q.models[modelKey(key)] == nil || q.models[modelKey(key)].shape != shape {
				t.Fatalf("%s block %d: read without a model", name, at.Number)
			}
			adopted++
		}
		t.Logf("%s: %d busy reads held at the chain's words, %d blocks whose last event does not state the pool", name, adopted, unstated)
		if adopted == 0 {
			t.Fatalf("%s: no busy read adopted", name)
		}
	}
}

func pairWord(reserve0, reserve1, high int64) domain.Hash {
	value := new(big.Int).Lsh(big.NewInt(high), 224)
	value.Or(value, new(big.Int).Lsh(big.NewInt(reserve1), 112)).Or(value, big.NewInt(reserve0))
	var packed domain.Hash
	value.FillBytes(packed[:])
	return packed
}

// A pair whose word holds something other than the time of its last update
// above the reserves (Camelot's pair keeps two fee percents there) gets no
// model, is not read again for one, and stays held by reads.
func TestPairWithoutUniswapsWordGetsNoModel(t *testing.T) {
	const now = 1794430000
	sync := func(address domain.Address, reserve0, reserve1 int64) []ingest.Transaction {
		first, second := slotKey(uint64(reserve0)), slotKey(uint64(reserve1))
		data := append(first[:], second[:]...)
		return []ingest.Transaction{{Successful: true, Logs: []ingest.Log{{Address: address, Topics: []domain.Hash{topicSync}, Data: data}}}}
	}
	for name, test := range map[string]struct {
		high   int64 // above the reserves in the word read
		synced bool  // the ring holds the pair's last Sync, at now-2
		fits   bool
	}{
		"fee percents":                    {high: 300<<16 | 300},
		"a time after the read block":     {high: now + 2},
		"not the time of its last Sync":   {high: now - 3, synced: true},
		"the time of its last Sync":       {high: now - 2, synced: true, fits: true},
		"an update before the ring began": {high: now - 500, fits: true},
	} {
		t.Run(name, func(t *testing.T) {
			q := newQuietPools(nil, func() uint64 { return 200 })
			key := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{9}}
			for number := uint64(190); number <= 200; number++ {
				var transactions []ingest.Transaction
				if test.synced && number == 193 {
					transactions = sync(key.Address, 5000, 7000)
				}
				q.observeBlock(domain.BlockRef{Number: number, Timestamp: now - 195 + number}, transactions)
			}
			state := pool.State{Key: key, Reserve0: big.NewInt(5000), Reserve1: big.NewInt(7000), UpdatedBlock: 100}
			entry := quietEntry{updated: 100, state: state, block: 196, slots: []execution.SlotCondition{{Slot: slotKey(8), Value: pairWord(5000, 7000, test.high)}}}
			q.mu.Lock()
			q.adoptRead(key, entry)
			q.entries[key] = entry
			q.mu.Unlock()
			if got := q.models[modelKey(key)] != nil; got != test.fits {
				t.Fatalf("model %v, want %v", got, test.fits)
			}
			// Either way the pair is held at the word that was read.
			if got := q.conditions(state); !slices.Equal(got, entry.slots) {
				t.Fatalf("held at %x, read %x", got, entry.slots)
			}
			// (A first use of a model queues its comparison with the chain.)
			queued := len(q.requests)
			q.warm([]pool.State{state})
			if len(q.requests) != queued {
				t.Fatal("a pair with a model, or one that cannot have one, was read again")
			}
		})
	}
}

// A model whose word differs from a later read of the chain is restarted; a
// pair's is not, since its word leaves nothing to doubt, unless the pair was
// synced after the read block: the same reserves at a later time.
func TestPairWhoseDerivedWordDiffersIsHeldByReadsOnly(t *testing.T) {
	f, address := loadWordFixture(t, "v2-uniswap-006a31cb.json")
	key := f.key(t, address)
	words := f.slots(t, f.Start)
	state := f.state(t, address, words, f.Start.Number)
	sync := ingest.Log{Address: address, Topics: []domain.Hash{topicSync}, Data: make([]byte, 64)}
	state.Reserve0.FillBytes(sync.Data[:32])
	state.Reserve1.FillBytes(sync.Data[32:])
	for name, syncedAfter := range map[string]bool{"quiet since the read": false, "synced after the read": true} {
		t.Run(name, func(t *testing.T) {
			q := newQuietPools(nil, func() uint64 { return f.Start.Number + 20 })
			for number := f.Start.Number; number <= f.Start.Number+20; number++ {
				var transactions []ingest.Transaction
				if syncedAfter && number == f.Start.Number+15 {
					transactions = []ingest.Transaction{{Successful: true, Logs: []ingest.Log{sync}}}
				}
				q.observeBlock(domain.BlockRef{Number: number, Timestamp: f.Start.Timestamp + (number-f.Start.Number)/4}, transactions)
				if number == f.Start.Number {
					q.mu.Lock()
					adopted := q.adopt(key, words, number)
					q.mu.Unlock()
					if !adopted {
						t.Fatal("start words not adopted")
					}
				}
			}
			q.mu.Lock()
			defer q.mu.Unlock()
			// The word read at a block before the Sync differs from the model's.
			read := quietEntry{state: state, block: f.Start.Number + 10, slots: []execution.SlotCondition{words[0]}}
			if !syncedAfter {
				read.slots[0].Value[0] ^= 0x01 // other bits above the reserves than the model has
			}
			q.adoptRead(key, read)
			if unfit := q.unfit[modelKey(key)]; unfit == syncedAfter {
				t.Fatalf("unfit %v", unfit)
			}
			if kept := q.models[modelKey(key)] != nil; kept != syncedAfter || q.derives(key) != syncedAfter {
				t.Fatalf("model kept %v", kept)
			}
		})
	}
}

// Models of every shape are saved and continue after a restart; a V4 pool's
// is saved under its ID.
func TestPairAndV4ModelsSurviveARestart(t *testing.T) {
	for _, name := range []string{"v2-sushi-905dfcd5.json", "v4-eth-usdc-500.json"} {
		f, address := loadWordFixture(t, name)
		key := f.key(t, address)
		path := filepath.Join(t.TempDir(), "reactive.jsonl.quiet.json")
		cut := len(f.Blocks) / 2
		// Only the blocks around the fixture's events are applied: the ring
		// starts again just before each one, as after a reseed.
		follow := func(q *quietPools, block wordFixtureBlock, model []execution.SlotCondition, since uint64) {
			q.observeBlock(domain.BlockRef{Number: block.Number - 1, Timestamp: since}, nil)
			if model != nil {
				q.mu.Lock()
				defer q.mu.Unlock()
				if !q.adopt(key, model, block.Number-1) {
					t.Fatalf("%s: words not adopted", name)
				}
				q.mu.Unlock()
				defer q.mu.Lock()
			}
			q.observeBlock(domain.BlockRef{Number: block.Number, Timestamp: block.Timestamp}, []ingest.Transaction{{Successful: true, Logs: block.logs(t, address)}})
		}
		q := newQuietPools(nil, func() uint64 { return f.Blocks[cut].Number })
		q.store = path
		follow(q, f.Blocks[cut], f.slots(t, f.Blocks[cut-1]), f.Blocks[cut-1].Timestamp)
		q.save()

		next := f.Blocks[cut+1]
		// The head is where the pool's last change has rested.
		restarted := newQuietPools(nil, func() uint64 { return next.Number + quietBlocks })
		restarted.store = path
		restarted.load()
		if !restarted.ready() || restarted.models[modelKey(key)] == nil {
			t.Fatalf("%s: model not restored", name)
		}
		// The saved model continues only from the saved block's child.
		for number := f.Blocks[cut].Number + 1; number < next.Number; number++ {
			restarted.observeBlock(domain.BlockRef{Number: number, Timestamp: f.Blocks[cut].Timestamp}, nil)
		}
		restarted.observeBlock(domain.BlockRef{Number: next.Number, Timestamp: next.Timestamp}, []ingest.Transaction{{Successful: true, Logs: next.logs(t, address)}})
		want := f.slots(t, next)
		if got := restarted.conditions(f.state(t, address, want, next.Number)); !slices.Equal(got, want) {
			t.Fatalf("%s: after a restart held at %x, chain %x", name, got, want)
		}
	}
}

// A file of the format before the models were named by pool and shape is
// ignored: the start is a first start.
func TestQuietStoreOfAnOlderFormatIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reactive.jsonl.quiet.json")
	old := `{"format":1,"block":100,"timestamp":5,"models":[{"address":"0x0300000000000000000000000000000000000000","price_slot":"0x00","liquidity_slot":"0x04","price":"5","tick":1,"index":1,"cardinality":2,"cardinality_next":2,"rest":"0","liquidity":"7","liquidity_high":"0","last":5}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	q := newQuietPools(nil, func() uint64 { return 100 })
	q.store = path
	q.load()
	if len(q.models) != 0 || len(q.recent) != 0 || q.ready() {
		t.Fatal("a file of an older format was restored")
	}
}
