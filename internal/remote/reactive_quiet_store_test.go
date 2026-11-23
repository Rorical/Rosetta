package remote

import (
	"context"
	"math/big"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

// If the word models lived only in memory, a restart inside a burst would send
// without pool conditions until they rebuilt, and such sends pay for lost
// races. The models are saved at the block they stand at and continue
// from the next applied block; a start from any other block drops them.
func TestQuietWordModelsSurviveARestart(t *testing.T) {
	f, address := loadWordFixture(t, "v3.json")
	path := filepath.Join(t.TempDir(), "reactive.jsonl.quiet.json")
	blocks := f.Blocks
	cut := len(blocks) / 2

	q := newQuietPools(nil, func() uint64 { return blocks[cut].Number })
	q.store = path
	q.observeBlock(domain.BlockRef{Number: f.Start.Number, Timestamp: f.Start.Timestamp}, nil)
	q.mu.Lock()
	adopted := q.adopt(f.key(t, address), f.slots(t, f.Start), f.Start.Number)
	q.mu.Unlock()
	if !adopted {
		t.Fatal("start words not adopted")
	}
	replay := func(q *quietPools, from, to int, timestamp uint64) uint64 {
		number := blocks[from].Number
		if from > 0 {
			number = blocks[from-1].Number + 1
		} else {
			number = f.Start.Number + 1
		}
		byNumber := map[uint64]wordFixtureBlock{}
		for _, block := range blocks[from:to] {
			byNumber[block.Number] = block
		}
		for ; number <= blocks[to-1].Number; number++ {
			var transactions []ingest.Transaction
			if block, ok := byNumber[number]; ok {
				timestamp = block.Timestamp
				transactions = []ingest.Transaction{{Successful: true, Logs: block.logs(t, address)}}
			}
			q.observeBlock(domain.BlockRef{Number: number, Timestamp: timestamp}, transactions)
		}
		return timestamp
	}
	timestamp := replay(q, 0, cut, f.Start.Timestamp)
	q.save()

	head := blocks[len(blocks)-1].Number + quietBlocks
	restarted := newQuietPools(nil, func() uint64 { return head })
	restarted.store = path
	restarted.load()
	if !restarted.ready() {
		t.Fatal("restored models are not ready to hold pools")
	}
	replay(restarted, cut, len(blocks), timestamp)
	want := f.slots(t, blocks[len(blocks)-1])
	state := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: address}, UpdatedBlock: blocks[len(blocks)-1].Number,
		SqrtPriceX96: new(big.Int).And(new(big.Int).SetBytes(want[0].Value[:]), mask160), Liquidity: new(big.Int).And(new(big.Int).SetBytes(want[1].Value[:]), mask128)}
	if got := restarted.conditions(state); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("after a restart held at %v, chain %v", got, want)
	}

	// A start that does not continue at the saved block has missed events.
	skipped := newQuietPools(nil, func() uint64 { return head })
	skipped.store = path
	skipped.load()
	skipped.observeBlock(domain.BlockRef{Number: blocks[cut-1].Number + 2, Timestamp: timestamp}, nil)
	if len(skipped.models) != 0 {
		t.Fatal("models kept across a gap in the applied blocks")
	}
}

// Without restored models nothing is sent until the first warm pass has read
// what it can: a route's pools would go out unconditioned.
func TestSendsWaitForTheWordModels(t *testing.T) {
	q := newQuietPools(nil, func() uint64 { return 100 })
	if q.ready() {
		t.Fatal("ready with no model and no warm pass")
	}
	pools := []pool.State{{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{1}}, SqrtPriceX96: big.NewInt(5), Liquidity: big.NewInt(7), UpdatedBlock: 50}}
	q.warm(pools)
	if q.ready() {
		t.Fatal("ready while the first reads are queued")
	}
	// The read finished (here: adopted a model), and the next pass finds nothing.
	<-q.requests
	q.mu.Lock()
	delete(q.queued, pools[0].Key)
	q.models[modelKey(pools[0].Key)] = &wordModel{}
	q.mu.Unlock()
	q.warm(pools)
	if !q.ready() {
		t.Fatal("not ready after a warm pass with nothing left to read")
	}
	// A reseed drops the models and sends wait again.
	q.reset()
	if q.ready() {
		t.Fatal("ready right after the models were dropped")
	}
	// The wait is bounded: a pool that cannot be read must not stop sending.
	q.mu.Lock()
	q.waitingSince = time.Now().Add(-quietReadyWait)
	q.mu.Unlock()
	if !q.ready() {
		t.Fatal("still waiting past the bound")
	}
}

// Conditions that were read (V2 reserves, V4 and Camelot words) are held only
// for the pool state they were read against, and that state, with the block
// it last changed in, is in the pool snapshot. So they survive a restart on
// the same terms, and a pool that changed while the service was down is read
// again, at the slots already known.
func TestReadConditionsAndLayoutsSurviveARestart(t *testing.T) {
	reserves := new(big.Int).Lsh(big.NewInt(7323762198), 112)
	reserves.Add(reserves, big.NewInt(41810613)).Add(reserves, new(big.Int).Lsh(big.NewInt(1794269756), 224))
	var packed domain.Hash
	reserves.FillBytes(packed[:])
	id := domain.Hash{0x02, 0xc0}
	v4 := v4Layout(id)
	server := &storageServer{words: map[domain.Hash]domain.Hash{slotKey(8): packed, v4[0]: word(5, 9000), v4[1]: word(0, 700)}, blocks: map[string]bool{}}
	path := filepath.Join(t.TempDir(), "reactive.jsonl.quiet.json")
	q := newQuietPools(server.serve(t), func() uint64 { return 100 })
	q.store = path
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { q.run(ctx); close(done) }()
	q.observeBlock(domain.BlockRef{Number: 100, Timestamp: 1}, nil)
	pair := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{1}}, Reserve0: big.NewInt(41810613), Reserve1: big.NewInt(7323762198), UpdatedBlock: 50}
	v4Pool := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{0x36}, PoolID: id}, SqrtPriceX96: big.NewInt(9000), Liquidity: big.NewInt(700), UpdatedBlock: 50}
	wantPair, wantV4 := waitHeld(t, q, pair), waitHeld(t, q, v4Pool)
	stop()
	<-done // the reader saves as it stops

	reads := server.count()
	restarted := newQuietPools(server.serve(t), func() uint64 { return 100 })
	restarted.store = path
	restarted.load()
	if got := restarted.conditions(pair); !reflect.DeepEqual(got, wantPair) {
		t.Fatalf("V2 reserves after a restart: %v, want %v", got, wantPair)
	}
	if got := restarted.conditions(v4Pool); !reflect.DeepEqual(got, wantV4) {
		t.Fatalf("V4 words after a restart: %v, want %v", got, wantV4)
	}
	if server.count() != reads {
		t.Fatal("restored conditions were read again")
	}
	// The pair traded while the service was down: its saved words are not
	// held, and the read that follows asks for the one known slot only.
	moved := pair.Copy()
	moved.UpdatedBlock, moved.Reserve0 = 60, big.NewInt(41810614)
	if restarted.conditions(moved) != nil {
		t.Fatal("words of an older pool state were held")
	}
	if layout := restarted.layouts[pair.Key]; len(layout) != 1 || layout[0] != slotKey(8) {
		t.Fatalf("layout after a restart: %v", layout)
	}
}
