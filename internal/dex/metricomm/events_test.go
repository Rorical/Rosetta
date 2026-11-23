package metricomm

import (
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"github.com/Rorical/Rosetta/internal/domain"
)

// eventsFixture holds blocks in which a pool emitted logs, with its storage
// words before and after (metricomm_fixture.py, events mode).
type eventsFixture struct {
	Pool       string            `json:"pool"`
	Immutables fixtureImmutables `json:"immutables"`
	Cases      []struct {
		BlockNumber  uint64   `json:"block_number"`
		SelectedAs   string   `json:"selected_as"`
		Transactions int      `json:"transactions"`
		WordsBefore  []string `json:"words_before"`
		Logs         []struct {
			Topics []string `json:"topics"`
			Data   string   `json:"data"`
		} `json:"logs"`
		// WordsChanged maps a position in WordsBefore to the word after the block.
		WordsChanged map[string]string `json:"words_changed"`
	} `json:"cases"`
}

func isEvents(name string) bool { return strings.HasSuffix(name, "-events.json") }

func (im fixtureImmutables) decode() Immutables {
	return Immutables{
		Token0ScaleMultiplier: *uint256.MustFromDecimal(im.Token0ScaleMultiplier),
		Token1ScaleMultiplier: *uint256.MustFromDecimal(im.Token1ScaleMultiplier),
		LowestBin:             im.LowestBin,
		HighestBin:            im.HighestBin,
	}
}

func mustBytes(t *testing.T, s string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestApplyLogReplaysRealBlocks replays every log a pool emitted in a block
// over the storage words of the block before and requires the words the
// chain stored after it, word for word.
func TestApplyLogReplaysRealBlocks(t *testing.T) {
	for _, f := range loadFixtures[eventsFixture](t, isEvents) {
		t.Run(f.Pool, func(t *testing.T) {
			immutables := f.Immutables.decode()
			var replayed, severalSwaps, crossings, staleBlocks, swapLogs int
			for _, c := range f.Cases {
				words := make([]domain.Hash, len(c.WordsBefore))
				for i, raw := range c.WordsBefore {
					words[i] = mustHash(t, raw)
				}
				want := slices.Clone(words)
				for position, raw := range c.WordsChanged {
					index, err := strconv.Atoi(position)
					if err != nil {
						t.Fatal(err)
					}
					want[index] = mustHash(t, raw)
				}
				before := slices.Clone(words)
				binBefore := decodeSlot0BinIdx(words[0])

				staleBlock, swaps := false, 0
				for i, log := range c.Logs {
					topics := make([]domain.Hash, len(log.Topics))
					for j, raw := range log.Topics {
						topics[j] = mustHash(t, raw)
					}
					input := slices.Clone(words)
					next, changed, stale, err := ApplyLog(words, immutables, topics, mustBytes(t, log.Data))
					if err != nil {
						t.Fatalf("block %d log %d: %v", c.BlockNumber, i, err)
					}
					if !slices.Equal(words, input) {
						t.Fatalf("block %d log %d: ApplyLog mutated its input", c.BlockNumber, i)
					}
					isSwapLog := topics[0] == TopicSwap || topics[0] == TopicBinSwapped
					// Every log in the fixtures is the pool's own: a swap log is applied, anything else is stale.
					if changed != isSwapLog || stale == isSwapLog || (!changed && !slices.Equal(next, words)) {
						t.Fatalf("block %d log %d (%s): changed %t, stale %t", c.BlockNumber, i, log.Topics[0][:10], changed, stale)
					}
					if topics[0] == TopicSwap {
						swaps++
					}
					staleBlock = staleBlock || stale
					words = next
				}
				swapLogs += swaps
				if staleBlock {
					// The replay stops following the pool here; the caller re-reads storage.
					staleBlocks++
					t.Logf("block %d: stale after an unmodelled event, %d words differ on chain",
						c.BlockNumber, len(c.WordsChanged))
					continue
				}
				for i := range want {
					if words[i] != want[i] {
						t.Fatalf("block %d (%s): word %d is %s, chain has %s (before: %s)",
							c.BlockNumber, c.SelectedAs, i, words[i].Hex(), want[i].Hex(), before[i].Hex())
					}
				}
				replayed++
				if swaps >= 2 {
					severalSwaps++
				}
				if decodeSlot0BinIdx(words[0]) != binBefore {
					crossings++
				}
			}
			t.Logf("%d blocks replayed word for word (%d Swap logs, %d blocks with several swaps, %d ending in another bin), %d stale blocks",
				replayed, swapLogs, severalSwaps, crossings, staleBlocks)
			if replayed < 30 || severalSwaps == 0 || crossings == 0 {
				t.Fatalf("fixture too thin: %d blocks, %d with several swaps, %d bin crossings", replayed, severalSwaps, crossings)
			}
		})
	}
}

func decodeSlot0BinIdx(slot0 domain.Hash) int16 {
	var state State
	state.ApplySlot0(slot0)
	return state.CurBinIdx
}

func TestApplyLogMarksOtherPoolEventsStale(t *testing.T) {
	immutables := Immutables{LowestBin: -1, HighestBin: 0}
	words := make([]domain.Hash, 4)
	for _, signature := range []string{
		"LiquidityAdded(address,uint80,int256[],(int256,int256)[],uint256[])",
		"LiquidityRemoved(address,uint80,int256[],(int256,int256)[],uint256[])",
		"ProtocolFeesCollected(uint256,uint256,uint256,uint256)",
		"FeeTransferFailed(address,address,uint256)",
		"SpreadFeeUpdated(uint24)",
		"NotionalFeeUpdated(uint24)",
		"PauseLevelUpdated(uint8,uint8)",
		"PriceProviderUpdated(address)",
		"BinAdditionalFeesUpdated(int16,uint16,uint16)",
		"BidAskRefAsymmetryUpdated(uint16)",
	} {
		next, changed, stale, err := ApplyLog(words, immutables, []domain.Hash{eventTopic(signature)}, nil)
		if err != nil || changed || !stale || !slices.Equal(next, words) {
			t.Errorf("%s: changed %t, stale %t, err %v", signature, changed, stale, err)
		}
	}
	// ERC-20 Transfer: not an event of the pool.
	foreign := eventTopic("Transfer(address,address,uint256)")
	for _, topics := range [][]domain.Hash{nil, {foreign}} {
		_, changed, stale, err := ApplyLog(words, immutables, topics, nil)
		if err != nil || changed || stale {
			t.Errorf("topics %v: changed %t, stale %t, err %v", topics, changed, stale, err)
		}
	}
}

func TestApplyLogRejectsLogsThatDoNotFit(t *testing.T) {
	immutables := Immutables{LowestBin: -1, HighestBin: 1}
	words := make([]domain.Hash, 5)
	// Bin 0 holds one unit of each token and is 100 wide; slot 0 sits in it.
	words[3] = domain.Hash{4: 0, 5: 100, 18: 1, 31: 1}
	words[1] = domain.Hash{15: 1, 31: 1}

	payload := func(fields ...*uint256.Int) []byte {
		var data []byte
		for _, field := range fields {
			raw := field.Bytes32()
			data = append(data, raw[:]...)
		}
		return data
	}
	signed := func(value int64) *uint256.Int {
		if value < 0 {
			return new(uint256.Int).Neg(uint256.NewInt(uint64(-value)))
		}
		return uint256.NewInt(uint64(value))
	}
	deltas := func(delta0, delta1 int64) *uint256.Int {
		high := new(uint256.Int).Lsh(signed(delta0), 128)
		return high.Or(high, new(uint256.Int).And(signed(delta1), maxUint128))
	}
	details := func(binIdx int16, pos uint64) *uint256.Int {
		packed := new(uint256.Int).Lsh(uint256.NewInt(uint64(uint16(binIdx))), 104)
		return packed.Or(packed, uint256.NewInt(pos))
	}

	tests := []struct {
		name   string
		words  []domain.Hash
		topic  domain.Hash
		data   []byte
		want   error
		wantOK bool
	}{
		{"bin swap inside the grid", words, TopicBinSwapped, payload(signed(0), deltas(-1, 5), new(uint256.Int)), nil, true},
		{"bin swap short payload", words, TopicBinSwapped, make([]byte, 64), ErrInvalidLog, false},
		{"bin swap below the grid", words, TopicBinSwapped, payload(signed(-2), deltas(0, 0), new(uint256.Int)), ErrInvalidLog, false},
		{"bin swap above the grid", words, TopicBinSwapped, payload(signed(2), deltas(0, 0), new(uint256.Int)), ErrInvalidLog, false},
		{"bin swap with a huge index", words, TopicBinSwapped, payload(new(uint256.Int).Lsh(one, 200), deltas(0, 0), new(uint256.Int)), ErrInvalidLog, false},
		{"bin swap draining more than the bin holds", words, TopicBinSwapped, payload(signed(0), deltas(-2, 5), new(uint256.Int)), ErrInvalidLog, false},
		{"bin swap on truncated words", words[:4], TopicBinSwapped, payload(signed(0), deltas(-1, 5), new(uint256.Int)), ErrInvalidState, false},
		{"swap inside the grid", words, TopicSwap, payload(details(1, 7), new(uint256.Int), new(uint256.Int)), nil, true},
		{"swap long payload", words, TopicSwap, make([]byte, 128), ErrInvalidLog, false},
		{"swap ending outside the grid", words, TopicSwap, payload(details(2, 7), new(uint256.Int), new(uint256.Int)), ErrInvalidLog, false},
		{"swap with unknown detail bits", words, TopicSwap, payload(new(uint256.Int).Lsh(one, 121), new(uint256.Int), new(uint256.Int)), ErrInvalidLog, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next, changed, stale, err := ApplyLog(test.words, immutables, []domain.Hash{test.topic}, test.data)
			if test.wantOK {
				if err != nil || !changed || stale {
					t.Fatalf("changed %t, stale %t, err %v", changed, stale, err)
				}
				return
			}
			if !errors.Is(err, test.want) || changed || stale || !slices.Equal(next, test.words) {
				t.Fatalf("changed %t, stale %t, err %v, want %v", changed, stale, err, test.want)
			}
		})
	}

	// Crossing from bin 0 up into bin 1 adds the length of bin 0 to the distance.
	next, _, _, err := ApplyLog(words, immutables, []domain.Hash{TopicSwap}, payload(details(1, 7), new(uint256.Int), new(uint256.Int)))
	if err != nil {
		t.Fatal(err)
	}
	var state State
	state.ApplySlot0(next[0])
	if state.CurBinIdx != 1 || state.CurPosInBin.Uint64() != 7 || state.CurBinDistFromProvidedPriceE6 != 100 {
		t.Fatalf("bin %d pos %s distance %d", state.CurBinIdx, &state.CurPosInBin, state.CurBinDistFromProvidedPriceE6)
	}
}
