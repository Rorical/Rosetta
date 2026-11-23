package metricomm

import (
	"encoding/binary"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// skewUpdateFixture records a real pool's oracle skew updates, read from an
// Arbitrum One archive node.
type skewUpdateFixture struct {
	Pool           string            `json:"pool"`
	Immutables     fixtureImmutables `json:"immutables"`
	ConfigWord     string            `json:"config_word"`
	ParametersWord string            `json:"parameters_word"`
	Cases          []struct {
		BlockNumber    uint64   `json:"block_number"`
		BlockTimestamp uint64   `json:"block_timestamp"`
		SelectedAs     string   `json:"selected_as"`
		StateBefore    string   `json:"state_before"`
		WordsBefore    []string `json:"words_before"`
		Logs           []struct {
			Topics []string `json:"topics"`
			Data   string   `json:"data"`
		} `json:"logs"`
		StateAfter string `json:"state_after"`
		Slot0After string `json:"slot0_after"`
	} `json:"cases"`
}

func isSkewUpdates(name string) bool { return strings.HasSuffix(name, "-skew-updates.json") }

// skewUpdateTally counts what the replayed blocks exercise, so a fixture that
// lost a class of cases fails instead of passing on less.
type skewUpdateTally struct {
	singleSwap, severalSwaps        int
	rewritten, kept                 int
	swapsUp, swapsDown              int
	swapsCrossingBins, swapsInBin   int
	levelAtTarget, levelMoving      int
	minElapsed, maxElapsed          uint64
	rewritesInSameBlock, oddEndBins int
	// keepOldErrorPpm is, per swap and per entry of keepOldHorizons, how far
	// the widening is off that many seconds after the swap when the block's
	// old state word is kept instead of the true one, the pool staying where
	// the swap left it.
	keepOldErrorPpm [len(keepOldHorizons)][]uint32
}

// keepOldHorizons are the delays, in seconds after a swap's block, at which
// the stale word is compared with the true one. Both words give the same
// level at the block itself; they part as the level relaxes to different
// targets.
var keepOldHorizons = [...]uint64{0, 1, 5, 60, 600}

func skewUpdatedAt(stateWord domain.Hash) uint64 {
	var seconds [8]byte
	copy(seconds[2:], stateWord[skewTimeOffset:skewTargetOffset])
	return binary.BigEndian.Uint64(seconds[:])
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestNextSkewStateMatchesChain replays the swaps of real blocks: every Swap
// log moves slot 0 through ApplyLog and the extension's word through
// NextSkewState, and the word after the last swap must be the one the chain
// stored at the block, bit for bit.
func TestNextSkewStateMatchesChain(t *testing.T) {
	fixtures := loadFixtures[skewUpdateFixture](t, isSkewUpdates)
	if len(fixtures) < 2 {
		t.Fatalf("%d skew update fixtures, want one per pool", len(fixtures))
	}
	for _, f := range fixtures {
		immutables := f.Immutables.decode()
		configWord, parametersWord := mustHash(t, f.ConfigWord), mustHash(t, f.ParametersWord)
		config := DecodeSkewConfig(configWord, parametersWord)
		tally := skewUpdateTally{minElapsed: ^uint64(0)}
		for _, c := range f.Cases {
			words := make([]domain.Hash, len(c.WordsBefore))
			for i, word := range c.WordsBefore {
				words[i] = mustHash(t, word)
			}
			before := mustHash(t, c.StateBefore)
			state, swaps, rewrites := before, 0, 0

			elapsed := c.BlockTimestamp - skewUpdatedAt(before)
			tally.minElapsed, tally.maxElapsed = min(tally.minElapsed, elapsed), max(tally.maxElapsed, elapsed)
			if elapsed >= uint64(config.RelaxSeconds) {
				tally.levelAtTarget++
			} else {
				tally.levelMoving++
			}

			for _, log := range c.Logs {
				topics := make([]domain.Hash, len(log.Topics))
				for i, topic := range log.Topics {
					topics[i] = mustHash(t, topic)
				}
				next, _, stale, err := ApplyLog(words, immutables, topics, mustBytes(t, log.Data))
				if err != nil || stale {
					t.Fatalf("%s block %d: log does not replay (stale %v): %v", f.Pool, c.BlockNumber, stale, err)
				}
				if topics[0] != TopicSwap {
					words = next
					continue
				}
				var from, to State
				from.ApplySlot0(words[slot0Index])
				to.ApplySlot0(next[slot0Index])
				words = next
				swaps++
				up := to.CurBinIdx > from.CurBinIdx ||
					(to.CurBinIdx == from.CurBinIdx && to.CurPosInBin.Gt(&from.CurPosInBin))
				if up {
					tally.swapsUp++
				} else {
					tally.swapsDown++
				}
				if to.CurBinIdx != from.CurBinIdx {
					tally.swapsCrossingBins++
				} else {
					tally.swapsInBin++
				}
				if to.CurBinIdx%2 != 0 {
					tally.oddEndBins++
				}

				updated := NextSkewState(state, words[slot0Index], configWord, parametersWord, c.BlockTimestamp)
				if updated != state {
					rewrites++
				}
				state = updated

				for i, delay := range keepOldHorizons {
					trueBid, trueAsk := SkewPpm(words[slot0Index], state, config, c.BlockTimestamp+delay)
					oldBid, oldAsk := SkewPpm(words[slot0Index], before, config, c.BlockTimestamp+delay)
					tally.keepOldErrorPpm[i] = append(tally.keepOldErrorPpm[i], max(absDiff(trueBid, oldBid), absDiff(trueAsk, oldAsk)))
				}
			}

			if state != mustHash(t, c.StateAfter) {
				t.Errorf("%s block %d (%s, %d swaps): state word %s, chain has %s",
					f.Pool, c.BlockNumber, c.SelectedAs, swaps, state, c.StateAfter)
			}
			if words[slot0Index] != mustHash(t, c.Slot0After) {
				t.Errorf("%s block %d: slot 0 %s, chain has %s", f.Pool, c.BlockNumber, words[slot0Index], c.Slot0After)
			}
			if swaps > 1 {
				tally.severalSwaps++
			} else {
				tally.singleSwap++
			}
			if rewrites > 0 {
				tally.rewritten++
			} else {
				tally.kept++
			}
			if rewrites > 1 {
				tally.rewritesInSameBlock++
			}
		}

		if tally.singleSwap < 60 || tally.severalSwaps < 15 {
			t.Errorf("%s: %d single-swap and %d several-swap blocks, want at least 60 and 15",
				f.Pool, tally.singleSwap, tally.severalSwaps)
		}
		for name, count := range map[string]int{
			"rewritten": tally.rewritten, "kept": tally.kept,
			"swaps up": tally.swapsUp, "swaps down": tally.swapsDown,
			"swaps crossing bins": tally.swapsCrossingBins, "swaps inside a bin": tally.swapsInBin,
			"level at target": tally.levelAtTarget, "level moving": tally.levelMoving,
			"odd end bins": tally.oddEndBins,
		} {
			if count == 0 {
				t.Errorf("%s: no case with %s", f.Pool, name)
			}
		}
		t.Logf("%s: blocks %d single + %d several; %d rewritten (%d twice in a block), %d kept; "+
			"swaps %d up, %d down, %d crossing bins, %d inside one; level at target %d, moving %d; "+
			"%d..%d s since the last update",
			f.Pool, tally.singleSwap, tally.severalSwaps, tally.rewritten, tally.rewritesInSameBlock, tally.kept,
			tally.swapsUp, tally.swapsDown, tally.swapsCrossingBins, tally.swapsInBin,
			tally.levelAtTarget, tally.levelMoving, tally.minElapsed, tally.maxElapsed)
		for i, delay := range keepOldHorizons {
			errors := tally.keepOldErrorPpm[i]
			slices.Sort(errors)
			quantile := func(q float64) uint32 { return errors[int(q*float64(len(errors)-1))] }
			wrong := len(errors) - sort.Search(len(errors), func(k int) bool { return errors[k] > 0 })
			t.Logf("%s: keeping the old word, %d s after the swap: off by p50 %d, p90 %d, max %d ppm; wrong for %d of %d swaps",
				f.Pool, delay, quantile(0.5), quantile(0.9), quantile(1), wrong, len(errors))
		}
	}
}
