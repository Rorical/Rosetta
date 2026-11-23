package exec

import (
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// DispatchStatus is the operator's view of the nonce lane and the daily fee
// budget. Amounts are native wei. It is read-only and carries no signed bytes.
type DispatchStatus struct {
	HaltedReason   string       `json:"halted_reason,omitempty"`
	Stopped        bool         `json:"stopped"`
	NextNonce      uint64       `json:"next_nonce"`
	Pending        *domain.Hash `json:"pending,omitempty"`
	Variants       int          `json:"variants,omitempty"`
	Included       int          `json:"included"`
	SpentToday     *big.Int     `json:"spent_today"`
	DailyCap       *big.Int     `json:"daily_cap"`
	PerTransaction *big.Int     `json:"transaction_cap"`
}

// Status waits for the send lock, so call it from the goroutine that sends and
// reconciles. A stop file that cannot be checked counts as present, as it
// blocks sends the same way.
func (d *Dispatcher) Status(now time.Time) DispatchStatus {
	d.mu.Lock()
	state := d.store.state
	d.mu.Unlock()
	status := DispatchStatus{HaltedReason: state.HaltedReason, NextNonce: state.NextNonce, Included: len(state.Included),
		SpentToday: new(big.Int).Set(state.Charged), DailyCap: new(big.Int).Set(d.limits.PerDay), PerTransaction: new(big.Int).Set(d.limits.PerTransaction)}
	// Dispatch opens a new day lazily; until then only in-flight reservations
	// carry over.
	if now.Unix()/86400 > state.Day {
		status.SpentToday = inFlight(state)
	}
	if state.Pending != nil {
		hash := state.Pending.Hash
		status.Pending, status.Variants = &hash, 1+len(state.Pending.Previous)
	}
	stopped, err := d.stopRequested()
	status.Stopped = stopped || err != nil
	return status
}
