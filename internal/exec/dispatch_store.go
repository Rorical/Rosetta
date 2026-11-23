package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/core/types"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/gofrs/flock"
)

type PendingTransaction struct {
	Cancellation *Cancellation `json:",omitempty"`
	// Maintenance marks an owner sweep or unwrap; Intent then only carries
	// the lane identity (Maintenance.carrier).
	Maintenance *Maintenance         `json:",omitempty"`
	Previous    []PendingTransaction `json:",omitempty"`
	Intent      ReactiveIntent
	Hash        domain.Hash
	Raw         []byte
	Nonce       uint64
	SubmittedMS int64
	// ReleasedMS is when the lane moved past this nonce on a feed inclusion;
	// Rebroadcasts counts resends of a transaction missing from the chain.
	ReleasedMS   int64 `json:",omitempty"`
	Rebroadcasts int   `json:",omitempty"`
	// Settlement is the outcome booked for this nonce from the feed or a
	// canonical block, SettledMS when it was first booked, and ObservedBlock
	// the block it was last seen in. Canonical marks a canonical block or
	// receipt containing it. CheckMS defers the next missing-transaction check.
	Settlement    *Settlement `json:",omitempty"`
	SettledMS     int64       `json:",omitempty"`
	ObservedBlock uint64      `json:",omitempty"`
	Canonical     bool        `json:",omitempty"`
	CheckMS       int64       `json:",omitempty"`
}

type DispatchState struct {
	HaltedReason string `json:",omitempty"`
	Chain        domain.ChainIdentity
	Sender       domain.Address
	Executor     domain.Address
	NextNonce    uint64
	Day          int64
	Charged      *big.Int
	Pending      *PendingTransaction
	Included     []PendingTransaction `json:",omitempty"` // nonce consumed; awaiting outcome, fees or canonical sighting
	Completed    []domain.Hash
	LastReceipt  *Settlement
}

type dispatchStore struct {
	path   string
	lock   *flock.Flock
	state  DispatchState
	failed bool
	limits DispatchLimits // validates persisted maintenance calldata
}

func openDispatchStore(path string, initial DispatchState, limits DispatchLimits) (*dispatchStore, error) {
	if path == "" {
		return nil, fmt.Errorf("dispatch journal path required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLock()
	if err != nil || !ok {
		return nil, fmt.Errorf("dispatch journal already owned or unavailable")
	}
	s := &dispatchStore{path: path, lock: lock, state: initial, limits: limits}
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &s.state)
		if err == nil && (s.state.Chain != initial.Chain || s.state.Sender != initial.Sender || s.state.Executor != initial.Executor || s.state.Charged == nil || s.state.Charged.Sign() < 0) {
			err = fmt.Errorf("dispatch journal identity or budget mismatch")
		}
	} else if os.IsNotExist(err) {
		err = s.save(initial)
	}
	if err == nil {
		err = s.validatePending()
	}
	if err != nil {
		lock.Unlock()
		return nil, err
	}
	return s, nil
}

// Any uncertain write poisons this owner. Restart reloads the actually durable
// record before another signature or send can occur.
func (s *dispatchStore) save(state DispatchState) (err error) {
	if s.failed {
		return fmt.Errorf("dispatch journal requires restart")
	}
	defer func() {
		if err != nil {
			s.failed = true
		}
	}()
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), ".dispatch-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	errClose := file.Close()
	if err != nil {
		return err
	}
	if errClose != nil {
		return errClose
	}
	if err = os.Rename(file.Name(), s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	errClose = dir.Close()
	if err != nil {
		return err
	}
	if errClose != nil {
		return errClose
	}
	s.state = state
	return nil
}
func (s *dispatchStore) close() error { return s.lock.Unlock() }

func (s *dispatchStore) validatePending() error {
	p := s.state.Pending
	if p == nil {
		return nil
	}
	if p.Cancellation != nil && len(p.Previous) == 0 {
		return fmt.Errorf("cancellation missing original variant")
	}
	if len(p.Previous) > 4 || p.Nonce != s.state.NextNonce || p.Nonce == ^uint64(0) {
		return fmt.Errorf("invalid persisted nonce lane")
	}
	if p.Maintenance != nil && (len(p.Previous) > 0 || p.Cancellation != nil) {
		return fmt.Errorf("maintenance cannot have variants")
	}
	variants := append([]PendingTransaction{*p}, p.Previous...)
	for _, variant := range variants {
		i := variant.Intent
		if i.Chain != s.state.Chain || i.Call.Sender != s.state.Sender || i.Call.Executor != s.state.Executor || variant.Nonce != p.Nonce {
			return fmt.Errorf("persisted transaction identity mismatch")
		}
		if variant.Maintenance != nil {
			if err := variant.Maintenance.validate(s.limits); err != nil {
				return fmt.Errorf("persisted maintenance: %w", err)
			}
			if variant.Maintenance.id() != i.ID {
				return fmt.Errorf("persisted maintenance identity mismatch")
			}
		} else if err := i.Validate(context.Background(), time.UnixMilli(i.CreatedMS)); err != nil {
			return fmt.Errorf("persisted intent: %w", err)
		}
		var signed types.Transaction
		if err := signed.UnmarshalBinary(variant.Raw); err != nil {
			return fmt.Errorf("persisted transaction decoding: %w", err)
		}
		if domain.Hash(signed.Hash()) != variant.Hash {
			return fmt.Errorf("persisted transaction hash mismatch")
		}
		if variant.Cancellation != nil {
			if err := variant.Cancellation.validate(time.UnixMilli(variant.Cancellation.CreatedMS)); err != nil {
				return err
			}
			if len(variant.Previous) > 0 && variant.Cancellation.Target != variant.Previous[len(variant.Previous)-1].Hash {
				return fmt.Errorf("cancellation target mismatch")
			}
		}
		expected := pendingEnvelope(variant)

		if err := verifySigned(expected, &signed, s.state.Sender); err != nil {
			return err
		}
	}
	if s.state.Charged.Cmp(pendingBudget(*p)) < 0 {
		return fmt.Errorf("persisted budget below pending envelope")
	}
	return nil
}
