package main

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func hash(block uint64, index int) domain.Hash {
	var h domain.Hash
	h[0], h[1], h[31] = byte(block>>8), byte(block), byte(index+1)
	return h
}

// chain builds canonical blocks with `system` leading receipts that the feed
// does not carry, then `user` receipts it does, at feed index start+i.
func chain(numbers []uint64, system, user int, start uint64) ([]observation, map[uint64]canonicalBlock) {
	var frames []observation
	blocks := make(map[uint64]canonicalBlock)
	for _, number := range numbers {
		block := canonicalBlock{Number: number}
		for i := range system + user {
			block.Hashes = append(block.Hashes, hash(number, i))
		}
		blocks[number] = block
		for i := range user {
			frames = append(frames, observation{BlockNumber: number, TxIndex: start + uint64(i), TxHash: hash(number, system+i), PGARound: uint64(1 + i%2), ReceivedMS: int64(100*number) + int64(i), TimestampMS: uint64(100 * number)})
		}
	}
	return frames, blocks
}

func TestAnalyzeFindsSystemOffset(t *testing.T) {
	tests := []struct {
		name                   string
		system                 int
		start                  uint64
		wantFast, wantCanon    uint64
		wantConsistent, blocks int
	}{
		{name: "feed skips the internal start-block tx", system: 1, start: 0, wantFast: 0, wantCanon: 1, wantConsistent: 3, blocks: 3},
		{name: "feed numbers like canonical", system: 1, start: 1, wantFast: 1, wantCanon: 1, wantConsistent: 3, blocks: 3},
		{name: "no system receipts", system: 0, start: 0, wantFast: 0, wantCanon: 0, wantConsistent: 3, blocks: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frames, blocks := chain([]uint64{10, 11, 12, 13}, tt.system, 3, tt.start)
			got := analyze(frames, blocks)
			// Block 10 is the joined-mid-stream block and never counts.
			if got.Blocks != tt.blocks || len(got.Mappings) != 1 {
				t.Fatalf("blocks %d mappings %+v", got.Blocks, got.Mappings)
			}
			m := got.Mappings[0]
			if m.FastFirst != tt.wantFast || m.CanonicalFirst != tt.wantCanon || got.Consistent != tt.wantConsistent || got.Unmatched != 0 || got.Omitted != 0 {
				t.Fatalf("got %+v", got)
			}
			if got.FramesPerBlock.P50 != 3 || got.RoundsPerBlock.Max != 2 || got.SpreadMS.Max != 2 {
				t.Fatalf("shape %+v", got)
			}
		})
	}
}

func TestAnalyzeReportsDisagreement(t *testing.T) {
	frames, blocks := chain([]uint64{20, 21, 22}, 1, 3, 0)
	// A tentative frame that never became final, and a canonical receipt the
	// feed never carried.
	frames = append(frames, observation{BlockNumber: 22, TxIndex: 3, TxHash: hash(99, 0)})
	block := blocks[21]
	block.Hashes = append(block.Hashes, hash(21, 50))
	blocks[21] = block
	got := analyze(frames, blocks)
	if got.Blocks != 2 || got.Consistent != 0 || got.Unmatched != 1 || got.Omitted != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestAnalyzeSkipsBlocksWithoutReceipts(t *testing.T) {
	frames, blocks := chain([]uint64{30, 31, 32}, 1, 2, 0)
	delete(blocks, 32)
	got := analyze(frames, blocks)
	if got.Blocks != 1 || got.Consistent != 1 {
		t.Fatalf("got %+v", got)
	}
}
