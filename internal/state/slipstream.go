package state

import (
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/domain"
)

func (s *Store) advanceSlipstream(ref domain.BlockRef) {
	for key, current := range s.pools {
		m := current.Slipstream
		if m == nil || !m.Valid || m.Block == ref.Number && m.Timestamp == uint32(ref.Timestamp) {
			continue
		}
		next := current.Copy()
		slipstream.AdvanceTime(&next, ref)
		s.recordUndo(key)
		s.pools[key] = next
		// Time-only oracle and block-cache transitions participate in dirty paths.
		s.dirty[key] = struct{}{}
	}
}

// ApplySlipstreamConfiguration invalidates affected local models before later
// logs in the same transaction. Canonical seeding restores a verified model;
// no RPC is issued by the receipt writer or preconfirmation path.
func (s *Store) ApplySlipstreamConfiguration(address domain.Address, topics []domain.Hash) {
	if !slipstream.ConfigurationEvent(topics) {
		return
	}
	for key, current := range s.pools {
		if !slipstream.AffectsConfiguration(current.Slipstream, address) {
			continue
		}
		next := current.Copy()
		next.Slipstream.Valid = false
		next.Slipstream.NeedsRefresh = true
		next.Slipstream.UnavailableReason = "configuration_changed"
		s.recordUndo(key)
		s.pools[key] = next
		s.dirty[key] = struct{}{}
	}
}
