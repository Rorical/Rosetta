package backtest

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
)

func TestReactionWindow(t *testing.T) {
	txs := []ingest.Transaction{{Index: 0, Hash: domain.Hash{1}}, {Index: 1, Hash: domain.Hash{2}}, {Index: 2, Hash: domain.Hash{3}}}
	first := PrefixEvidence{First: 0, Count: 2, Hashes: []domain.Hash{{1}, {2}}, Received: 10, Applied: 20}
	last := PrefixEvidence{First: 2, Count: 1, Hashes: []domain.Hash{{3}}, Received: 50, Applied: 60}
	tests := []struct {
		name     string
		frames   []PrefixEvidence
		positive map[uint64]bool
		gaps     []TraceGap
		status   string
	}{
		{"observable", []PrefixEvidence{first, last}, map[uint64]bool{2: true}, nil, "observable"},
		{"same frame", []PrefixEvidence{{First: 0, Count: 3, Hashes: []domain.Hash{{1}, {2}, {3}}, Received: 10, Applied: 20}}, map[uint64]bool{2: true}, nil, "same_frame_no_window"},
		{"gap", []PrefixEvidence{first, last}, map[uint64]bool{2: true}, []TraceGap{{25, 30}}, "trace_gap"},
		{"old missing identities", []PrefixEvidence{{First: 0, Count: 2, Received: 10, Applied: 20}, last}, map[uint64]bool{2: true}, nil, "no_positive_visible_prefix"},
		{"reorg", []PrefixEvidence{{First: 0, Count: 2, Hashes: []domain.Hash{{4}, {2}}, Received: 10, Applied: 20}, last}, map[uint64]bool{2: true}, nil, "no_positive_visible_prefix"},
		{"late", []PrefixEvidence{{First: 0, Count: 2, Hashes: first.Hashes, Received: 10, Applied: 55}, last}, map[uint64]bool{2: true}, nil, "no_reaction_window"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReactionWindow(txs, 2, tt.frames, tt.positive, tt.gaps, nil)
			if got.Status != tt.status {
				t.Fatalf("%+v", got)
			}
		})
	}
}

// A backrun of the parent block's last round lands first in the next block:
// on Arbitrum One the ArbOS start-of-block transaction at index 0 arrives in
// the same batch as the competitor, so measuring only from the competitor's
// block leaves 1–3 ms for routes that have been visible for about 120 ms.
func TestReactionWindowStartsInTheParentBlock(t *testing.T) {
	txs := []ingest.Transaction{{Index: 0, Hash: domain.Hash{9}}, {Index: 1, Hash: domain.Hash{10}}}
	internal := PrefixEvidence{First: 0, Count: 1, Hashes: []domain.Hash{{9}}, Received: 1000, Applied: 1001}
	competitor := PrefixEvidence{First: 1, Count: 1, Hashes: []domain.Hash{{10}}, Received: 1002, Applied: 1003}
	parentTxs := []ingest.Transaction{{Index: 0, Hash: domain.Hash{1}}, {Index: 1, Hash: domain.Hash{2}}, {Index: 2, Hash: domain.Hash{3}}}
	frame := func(i uint64, at int64) PrefixEvidence {
		return PrefixEvidence{Version: domain.StateVersion{TxIndex: i}, First: i, Count: 1, Hashes: []domain.Hash{parentTxs[i].Hash}, Received: at, Applied: at + 1}
	}
	parentFrames := []PrefixEvidence{frame(0, 100), frame(1, 600), frame(2, 880)}
	for _, tt := range []struct {
		name         string
		start        bool
		parent       map[uint64]bool
		window       int64
		parentPrefix uint64
		fromParent   bool
		status       string
	}{
		{name: "trigger in the parent's last frame", start: true, parent: map[uint64]bool{3: true}, window: 1002 - 881, parentPrefix: 3, fromParent: true, status: "observable"},
		{name: "positive since an earlier parent frame", start: true, parent: map[uint64]bool{2: true, 3: true}, window: 1002 - 601, parentPrefix: 2, fromParent: true, status: "observable"},
		{name: "a broken run starts again", start: true, parent: map[uint64]bool{1: true, 2: false, 3: true}, window: 1002 - 881, parentPrefix: 3, fromParent: true, status: "observable"},
		{name: "not positive at the block start", parent: map[uint64]bool{3: true}, window: 1002 - 1001, status: "observable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			positive := map[uint64]bool{0: tt.start, 1: true}
			got := ReactionWindow(txs, 1, []PrefixEvidence{internal, competitor}, positive, nil, &ParentPrefixes{Transactions: parentTxs, Frames: parentFrames, Positive: tt.parent})
			if got.Status != tt.status || got.Window != tt.window || got.Parent != tt.fromParent || tt.fromParent && got.Prefix != tt.parentPrefix {
				t.Fatalf("%+v", got)
			}
		})
	}
}
