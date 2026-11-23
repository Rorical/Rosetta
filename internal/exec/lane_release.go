package exec

import (
	"math/big"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// maxIncluded bounds released nonces that still await their outcome, paid
// fees or canonical sighting. A full list keeps the lane closed.
const maxIncluded = 8

// releaseIncluded opens the nonce lane once any variant of the pending
// transaction appeared in an accepted prefix: that nonce is consumed, so the
// next one can be sent before the sighting is booked. The released
// transaction keeps its full fee reservation until it settles.
func releaseIncluded(state DispatchState, now time.Time) DispatchState {
	released := *state.Pending
	released.ReleasedMS = now.UnixMilli()
	state.Included = append(slices.Clone(state.Included), released)
	state.NextNonce = released.Nonce + 1
	state.Pending = nil
	return state
}

// inFlight is the reservation of transactions without a booked outcome. It
// opens every new day's budget, so a day boundary never forgets them. A booked
// transaction stays charged to the day it settled.
func inFlight(state DispatchState) *big.Int {
	total := new(big.Int)
	for _, entry := range state.Included {
		if entry.Settlement == nil {
			total.Add(total, pendingBudget(entry))
		}
	}
	if state.Pending != nil {
		total.Add(total, pendingBudget(*state.Pending))
	}
	return total
}

// sentIntent reports whether id was already signed for an unfinished nonce.
func sentIntent(state DispatchState, id [32]byte) bool {
	entries := slices.Clone(state.Included)
	if state.Pending != nil {
		entries = append(entries, *state.Pending)
	}
	for _, entry := range entries {
		for _, variant := range variants(entry) {
			if variant.Intent.ID == id {
				return true
			}
		}
	}
	return false
}

// variants lists every signed transaction of one nonce, newest first.
func variants(p PendingTransaction) []PendingTransaction {
	return append([]PendingTransaction{p}, p.Previous...)
}

func variantOf(p PendingTransaction, hash domain.Hash) (PendingTransaction, bool) {
	for _, variant := range variants(p) {
		if variant.Hash == hash {
			return variant, true
		}
	}
	return PendingTransaction{}, false
}
