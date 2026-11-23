package backtest

import (
	"cmp"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
)

// PrefixEvidence uses canonical transaction positions and exact identities.
// Version alone is insufficient: a preconfirmation branch can be replaced.
type PrefixEvidence struct {
	Version           domain.StateVersion
	First, Count      uint64
	Hashes            []domain.Hash
	Received, Applied int64
}
type TraceGap struct{ First, Last int64 }
type Visibility struct {
	Received int64  `json:"prefix_received_at_ns,omitempty"`
	Status   string `json:"status"`
	Prefix   uint64 `json:"before_tx,omitempty"`
	// Parent marks a prefix of the competitor's parent block: Prefix and
	// Version then refer to that block.
	Parent    bool                `json:"parent_block,omitempty"`
	Available int64               `json:"available_at_ns,omitempty"`
	Deadline  int64               `json:"competitor_received_at_ns,omitempty"`
	Window    int64               `json:"window_ns,omitempty"`
	Version   domain.StateVersion `json:"basis_version"`
}

// ParentPrefixes are the competitor's parent block, its frames and whether
// the route was positive after each frame end.
type ParentPrefixes struct {
	Transactions []ingest.Transaction
	Frames       []PrefixEvidence
	Positive     map[uint64]bool
}

// visibleSince is the earliest parent frame after which the route stayed
// positive through the parent's last transaction, over contiguous frames.
func (p *ParentPrefixes) visibleSince() (PrefixEvidence, bool) {
	var frames []PrefixEvidence
	for _, f := range p.Frames {
		if f.Count > 0 && f.Matches(p.Transactions) {
			frames = append(frames, f)
		}
	}
	slices.SortFunc(frames, func(a, b PrefixEvidence) int { return cmp.Compare(a.First, b.First) })
	var since PrefixEvidence
	found, end := false, uint64(len(p.Transactions))
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		if f.First+f.Count != end || !p.Positive[end] {
			break
		}
		since, found, end = f, true, f.First
	}
	return since, found
}

func (p PrefixEvidence) Matches(transactions []ingest.Transaction) bool {
	if p.Count != uint64(len(p.Hashes)) || p.First > uint64(len(transactions)) || p.Count > uint64(len(transactions))-p.First || p.Received <= 0 || p.Applied < p.Received {
		return false
	}
	for i, hash := range p.Hashes {
		tx := transactions[p.First+uint64(i)]
		if tx.Index != p.First+uint64(i) || tx.Hash != hash {
			return false
		}
	}
	return true
}

// ReactionWindow requires positive local quotes at whole visible prefixes.
// Competitor arrival is an observation deadline, not its private submission time.
// A route positive at the start of the competitor's block may have been
// visible since a frame of its parent, when parent is given: a backrun of the
// parent's last round lands first in the next block, behind only Arbitrum's
// start-of-block transaction, which arrives in the competitor's batch.
func ReactionWindow(transactions []ingest.Transaction, competitor uint64, frames []PrefixEvidence, positive map[uint64]bool, gaps []TraceGap, parent *ParentPrefixes) Visibility {
	result := Visibility{Status: "capture_incomplete"}
	var deadline int64
	for _, f := range frames {
		if f.Matches(transactions) && f.Count > 0 && f.First <= competitor && competitor-f.First < f.Count && (deadline == 0 || f.Received < deadline) {
			deadline = f.Received
		}
	}
	if deadline == 0 {
		return result
	}
	result.Deadline = deadline
	foundPrior := false
	for _, f := range frames {
		if !f.Matches(transactions) {
			continue
		}
		end := f.First + f.Count
		if end > competitor || !positive[end] {
			continue
		}
		foundPrior = true
		if f.Applied >= deadline {
			continue
		}
		if result.Available == 0 || f.Applied < result.Available {
			result.Available = f.Applied
			result.Received = f.Received
			result.Prefix = end
			result.Version = f.Version
		}
	}
	if parent != nil && positive[0] {
		if f, ok := parent.visibleSince(); ok {
			foundPrior = true
			if f.Applied < deadline && (result.Available == 0 || f.Applied < result.Available) {
				result.Available, result.Received, result.Prefix, result.Version, result.Parent = f.Applied, f.Received, f.First+f.Count, f.Version, true
			}
		}
	}
	if result.Available == 0 {
		result.Status = "no_positive_visible_prefix"
		if foundPrior {
			result.Status = "no_reaction_window"
		}
		for _, f := range frames {
			if f.Matches(transactions) && f.Count > 0 && f.First <= competitor && competitor-f.First < f.Count {
				for prefix, yes := range positive {
					if yes && prefix > f.First && prefix <= competitor {
						result.Status = "same_frame_no_window"
					}
				}
			}
		}
		return result
	}
	for _, g := range gaps {
		if g.First == 0 || g.Last == 0 || g.First <= deadline && g.Last >= result.Available {
			result.Status = "trace_gap"
			return result
		}
	}
	result.Status = "observable"
	result.Window = deadline - result.Available
	return result
}
