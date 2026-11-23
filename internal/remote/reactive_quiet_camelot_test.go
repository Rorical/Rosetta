package remote

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
)

// Six pools of the Camelot V3 factory. 0f143fc5 and 299c7d6f changed their
// fee inside the run; 299c7d6f is a pool rivals trade into and had 23 blocks
// with events in 500,000.
var camelotFixtures = []string{"camelot-b1026b8e.json", "camelot-e51635ae.json", "camelot-fae2ae0a.json", "camelot-9b6ff025.json", "camelot-0f143fc5.json", "camelot-299c7d6f.json"}

// A rival selling into a Camelot pool ahead of a route in the same round
// makes the route pay for both swaps before failing its profit check unless
// the send carries a condition on the pool. Words read a few blocks behind
// the head need the pool unchanged for quietBlocks, so Camelot words follow
// from the pool's events instead: every block of every
// fixture must give the chain's two words, with no restart.
func TestCamelotWordsMatchTheChain(t *testing.T) {
	type tally struct{ blocks, swaps, severalSwaps, fees, newFees, noFee, inRange, outOfRange, zero int }
	var total tally
	for _, name := range camelotFixtures {
		f, address := loadWordFixture(t, name)
		if f.shape(t) != shapeCamelot || f.Slots[0] != slotKey(2).Hex() || f.Slots[1] != slotKey(3).Hex() {
			t.Fatalf("%s: not a Camelot fixture at slots 2 and 3", name)
		}
		m := newWordModel(shapeCamelot, f.slots(t, f.Start), f.Start.Timestamp)
		if m == nil {
			t.Fatalf("%s: no model from the start words", name)
		}
		before := total
		for _, block := range f.Blocks {
			swaps, fees := 0, 0
			for _, log := range block.logs(t, address) {
				switch log.Topics[0] {
				case topicSwap:
					swaps++
				case topicCamelotFee:
					fees++
					feeZto, _ := dataWord(log.Data, 0)
					feeOtz, _ := dataWord(log.Data, 1)
					if feeZto.Uint64() != uint64(m.feeZto) || feeOtz.Uint64() != uint64(m.feeOtz) {
						total.newFees++
					}
				case topicMint, topicBurn:
					at := 0
					if log.Topics[0] == topicMint {
						at = 1
					}
					amount, _ := dataWord(log.Data, at)
					bottom, top := tick24(log.Topics[2][:]), tick24(log.Topics[3][:])
					switch {
					case amount.Sign() == 0:
						total.zero++
					case m.tick >= bottom && m.tick < top:
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
			total.swaps += swaps
			total.fees += fees
			if swaps > 1 {
				total.severalSwaps++
			}
			if fees == 0 {
				total.noFee++
			}
		}
		t.Logf("%s: %d blocks equal to the chain", name, total.blocks-before.blocks)
	}
	t.Logf("Camelot: %+v", total)
	if total.blocks < 370 || total.severalSwaps == 0 || total.newFees == 0 || total.noFee == 0 || total.inRange == 0 || total.outOfRange == 0 {
		t.Fatalf("Camelot fixtures cover too little: %+v", total)
	}
}

// Words that are not Algebra's directional-fee globalState get no model: a
// locked pool, an uninitialized one, and the six-field globalState of an older
// Algebra pool, whose lock is the byte at bit 232.
func TestCamelotModelNeedsTheDirectionalFeeLayout(t *testing.T) {
	f, _ := loadWordFixture(t, "camelot-b1026b8e.json")
	if newWordModel(shapeCamelot, f.slots(t, f.Start), 0) == nil {
		t.Fatal("no model from a pool's words")
	}
	for name, change := range map[string]func(word *domain.Hash){
		"locked":        func(word *domain.Hash) { word[0] = 0 },
		"no price":      func(word *domain.Hash) { clear(word[12:]) },
		"older Algebra": func(word *domain.Hash) { word[0], word[1], word[2] = 0, 0, 1 },
	} {
		slots := f.slots(t, f.Start)
		change(&slots[0].Value)
		if newWordModel(shapeCamelot, slots, 0) != nil {
			t.Fatalf("%s: a model from words of another layout", name)
		}
	}
}

// A community fee change rewrites bits the model keeps as read, and an event
// that does not parse says nothing: both drop the model for the next read.
func TestCamelotModelIsDroppedByWhatItCannotFollow(t *testing.T) {
	f, address := loadWordFixture(t, "camelot-b1026b8e.json")
	for name, log := range map[string]ingest.Log{
		"community fee": {Address: address, Topics: []domain.Hash{topicCamelotCommunityFee}, Data: make([]byte, 64)},
		"short fee":     {Address: address, Topics: []domain.Hash{topicCamelotFee}, Data: make([]byte, 32)},
		"short swap":    {Address: address, Topics: []domain.Hash{topicSwap}, Data: make([]byte, 128)},
	} {
		m := newWordModel(shapeCamelot, f.slots(t, f.Start), 0)
		if m.apply(log, 0) {
			t.Fatalf("%s: model kept", name)
		}
	}
}

// A Camelot model is saved with its fees and continues after a restart.
func TestCamelotModelSurvivesARestart(t *testing.T) {
	f, address := loadWordFixture(t, "camelot-0f143fc5.json")
	key := f.key(t, address)
	path := filepath.Join(t.TempDir(), "reactive.jsonl.quiet.json")
	cut := len(f.Blocks) / 2
	at := f.Blocks[cut]
	q := newQuietPools(nil, func() uint64 { return at.Number })
	q.store = path
	q.observeBlock(domain.BlockRef{Number: at.Number, Timestamp: at.Timestamp}, nil)
	q.mu.Lock()
	adopted := q.adopt(key, f.slots(t, at), at.Number)
	q.mu.Unlock()
	if !adopted {
		t.Fatal("words not adopted")
	}
	q.save()

	// The head is where every change of the fixture has rested.
	restarted := newQuietPools(nil, func() uint64 { return f.Blocks[len(f.Blocks)-1].Number + quietBlocks })
	restarted.store = path
	restarted.load()
	if m := restarted.models[modelKey(key)]; m == nil || m.shape != shapeCamelot || !slices.Equal(m.conditions(), f.slots(t, at)) {
		t.Fatal("model not restored at the saved words")
	}
	number := at.Number
	for _, block := range f.Blocks[cut+1:] {
		for number++; number < block.Number; number++ {
			restarted.observeBlock(domain.BlockRef{Number: number, Timestamp: block.Timestamp}, nil)
		}
		restarted.observeBlock(domain.BlockRef{Number: block.Number, Timestamp: block.Timestamp}, []ingest.Transaction{{Successful: true, Logs: block.logs(t, address)}})
		want := f.slots(t, block)
		if got := restarted.conditions(f.state(t, address, want, block.Number)); !slices.Equal(got, want) {
			t.Fatalf("block %d: after a restart held at %x, chain %x", block.Number, got, want)
		}
	}
}
