package main

import (
	"cmp"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// observation is one Fast Feed frame as received here.
type observation struct {
	ReceivedMS  int64       `json:"received_ms"`
	TimestampMS uint64      `json:"timestamp_ms"`
	PGARound    uint64      `json:"pga_round"`
	BlockNumber uint64      `json:"block"`
	TxIndex     uint64      `json:"tx_index"`
	TxHash      domain.Hash `json:"tx_hash"`
	Logs        int         `json:"logs"`
}

// canonicalBlock lists a block's receipt hashes in canonical index order.
type canonicalBlock struct {
	Number uint64
	Hashes []domain.Hash
}

// mapping is one candidate value for -fast-first-index and
// -fast-canonical-first-index.
type mapping struct {
	FastFirst      uint64 `json:"fast_first_index"`
	CanonicalFirst uint64 `json:"canonical_first_index"`
	Blocks         int    `json:"blocks"`
}

type quantiles struct {
	P10 float64 `json:"p10"`
	P50 float64 `json:"p50"`
	P90 float64 `json:"p90"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

type report struct {
	Messages int `json:"messages"`
	// Blocks counts complete blocks: seen from their first frame and with a
	// canonical receipt list. The first and last blocks of a run are partial.
	Blocks   int       `json:"blocks"`
	Mappings []mapping `json:"mappings"`
	// Consistent counts blocks whose every frame matches the canonical receipt
	// at the recommended mapping; a block with any mismatch is not counted.
	Consistent int `json:"consistent_blocks"`
	// Unmatched counts frames whose hash is absent from the canonical block
	// they named: tentative positions that did not become final.
	Unmatched int `json:"unmatched_frames"`
	// Omitted counts canonical receipts at or after the mapped first index
	// that no frame carried.
	Omitted        int       `json:"omitted_receipts"`
	FramesPerBlock quantiles `json:"frames_per_block"`
	RoundsPerBlock quantiles `json:"rounds_per_block"`
	// ArrivalMS is local receive time minus the frame's timestamp_ms. It
	// includes clock offset between this host and the sequencer.
	ArrivalMS quantiles `json:"arrival_ms"`
	// SpreadMS is the receive-time span from a block's first to last frame.
	SpreadMS quantiles `json:"block_spread_ms"`
}

// analyze derives the index mapping from transaction hashes and checks it on
// every complete block. It trusts nothing about where system transactions sit.
func analyze(frames []observation, blocks map[uint64]canonicalBlock) report {
	out := report{Messages: len(frames)}
	byBlock := make(map[uint64][]observation)
	for _, frame := range frames {
		byBlock[frame.BlockNumber] = append(byBlock[frame.BlockNumber], frame)
	}
	numbers := make([]uint64, 0, len(byBlock))
	for number := range byBlock {
		numbers = append(numbers, number)
	}
	slices.Sort(numbers)
	// The first block was joined mid-stream; its first frame is not the block's.
	if len(numbers) > 0 {
		numbers = numbers[1:]
	}
	counts := make(map[[2]uint64]int)
	var complete []uint64
	var perBlock, rounds, spread, arrival []float64
	for _, number := range numbers {
		canonical, ok := blocks[number]
		if !ok {
			continue
		}
		seen := byBlock[number]
		slices.SortFunc(seen, func(a, b observation) int { return cmp.Compare(a.TxIndex, b.TxIndex) })
		first := seen[0]
		position := slices.Index(canonical.Hashes, first.TxHash)
		if position < 0 {
			continue
		}
		counts[[2]uint64{first.TxIndex, uint64(position)}]++
		complete = append(complete, number)
		perBlock = append(perBlock, float64(len(seen)))
		distinct := make(map[uint64]bool)
		low, high := seen[0].ReceivedMS, seen[0].ReceivedMS
		for _, frame := range seen {
			distinct[frame.PGARound] = true
			low, high = min(low, frame.ReceivedMS), max(high, frame.ReceivedMS)
			arrival = append(arrival, float64(frame.ReceivedMS-int64(frame.TimestampMS)))
		}
		rounds = append(rounds, float64(len(distinct)))
		spread = append(spread, float64(high-low))
	}
	out.Blocks = len(complete)
	for key, n := range counts {
		out.Mappings = append(out.Mappings, mapping{FastFirst: key[0], CanonicalFirst: key[1], Blocks: n})
	}
	slices.SortFunc(out.Mappings, func(a, b mapping) int {
		if c := cmp.Compare(b.Blocks, a.Blocks); c != 0 {
			return c
		}
		return cmp.Compare(a.FastFirst, b.FastFirst)
	})
	if len(out.Mappings) > 0 {
		best := out.Mappings[0]
		for _, number := range complete {
			matched, unmatched, omitted := check(byBlock[number], blocks[number].Hashes, best)
			out.Unmatched += unmatched
			out.Omitted += omitted
			if matched {
				out.Consistent++
			}
		}
	}
	out.FramesPerBlock = summarize(perBlock)
	out.RoundsPerBlock = summarize(rounds)
	out.ArrivalMS = summarize(arrival)
	out.SpreadMS = summarize(spread)
	return out
}

// check reports whether every frame sits at its mapped canonical position,
// how many frames are absent from the block, and how many mapped receipts no
// frame carried.
func check(frames []observation, hashes []domain.Hash, m mapping) (bool, int, int) {
	matched := true
	unmatched := 0
	carried := make(map[uint64]bool)
	for _, frame := range frames {
		if !slices.Contains(hashes, frame.TxHash) {
			unmatched++
			matched = false
			continue
		}
		if frame.TxIndex < m.FastFirst {
			matched = false
			continue
		}
		index := m.CanonicalFirst + frame.TxIndex - m.FastFirst
		if index >= uint64(len(hashes)) || hashes[index] != frame.TxHash {
			matched = false
			continue
		}
		carried[index] = true
	}
	omitted := 0
	for index := m.CanonicalFirst; index < uint64(len(hashes)); index++ {
		if !carried[index] {
			omitted++
		}
	}
	return matched && omitted == 0, unmatched, omitted
}

func summarize(values []float64) quantiles {
	if len(values) == 0 {
		return quantiles{}
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	at := func(f float64) float64 { return sorted[min(len(sorted)-1, int(f*float64(len(sorted))))] }
	return quantiles{P10: at(0.10), P50: at(0.50), P90: at(0.90), P99: at(0.99), Max: sorted[len(sorted)-1]}
}
