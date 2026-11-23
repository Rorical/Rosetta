// Package poolrisk quarantines pools whose executed routes keep reverting
// although the local quote was positive. The executor admits any pool on
// chain, so this local list is what keeps a misbehaving pool out of search.
package poolrisk

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/gofrs/flock"
)

var ErrQuarantined = errors.New("pool quarantined after repeated execution reverts")

// Evidence is one quarantine decision. Reverts and routes count the executed
// samples that implicated the pool before it was quarantined.
type Evidence struct {
	Pool    domain.PoolKey  `json:"pool"`
	Reverts int             `json:"reverts"`
	Routes  int             `json:"routes"`
	Block   domain.BlockRef `json:"block"`
	Source  string          `json:"source"`
}

type record struct {
	Chain    domain.ChainIdentity `json:"chain"`
	Evidence Evidence             `json:"evidence"`
}

type quarantine struct {
	pools   map[domain.PoolKey]bool
	failure error
}

// Registry publishes immutable sets so search reads never take the
// persistence lock. Entries are sticky; removing one is an operator action.
type Registry struct {
	mu      sync.Mutex
	chain   domain.ChainIdentity
	current atomic.Pointer[quarantine]
	file    *os.File
	lock    *flock.Flock
}

func Open(path string, chain domain.ChainIdentity) (*Registry, error) {
	if chain.ChainID == 0 || (chain.Family != domain.FamilyOPStack && chain.Family != domain.FamilyNitro) {
		return nil, fmt.Errorf("pool risk registry requires a chain")
	}
	r := &Registry{chain: chain}
	set := &quarantine{pools: make(map[domain.PoolKey]bool)}
	r.current.Store(set)
	if path == "" {
		return r, nil
	}
	r.lock = flock.New(path + ".lock")
	ok, err := r.lock.TryLock()
	if err != nil || !ok {
		return nil, fmt.Errorf("pool risk registry already owned or unavailable")
	}
	r.file, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		r.lock.Unlock()
		return nil, err
	}
	scanner := bufio.NewScanner(r.file)
	for scanner.Scan() {
		var row record
		if err = json.Unmarshal(scanner.Bytes(), &row); err != nil {
			break
		}
		if row.Chain != chain {
			err = fmt.Errorf("pool risk registry chain mismatch")
			break
		}
		if err = validate(row.Evidence); err != nil {
			break
		}
		set.pools[row.Evidence.Pool] = true
	}
	if err == nil {
		err = scanner.Err()
	}
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("load pool risk registry: %w", err)
	}
	// Make a newly created journal's directory entry durable before Record
	// acknowledges evidence.
	directory, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = directory.Sync()
		err = errors.Join(err, directory.Close())
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func validate(e Evidence) error {
	if e.Pool.Address == (domain.Address{}) || e.Reverts < 1 || e.Routes < 1 || e.Block.Hash == (domain.Hash{}) || (e.Source != "execution_paper" && e.Source != "reactive_preflight") {
		return fmt.Errorf("invalid pool quarantine evidence")
	}
	return nil
}

func (r *Registry) Check(key domain.PoolKey) error {
	if r == nil {
		return nil
	}
	set := r.current.Load()
	if set.failure != nil {
		return set.failure
	}
	if set.pools[key] {
		return fmt.Errorf("%w: %s", ErrQuarantined, key.Address)
	}
	return nil
}

func (r *Registry) Record(e Evidence) error {
	if err := validate(e); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.current.Load()
	if old.failure != nil {
		return old.failure
	}
	if old.pools[e.Pool] {
		return nil
	}
	next := &quarantine{pools: make(map[domain.PoolKey]bool, len(old.pools)+1)}
	for key := range old.pools {
		next.pools[key] = true
	}
	next.pools[e.Pool] = true
	// Publish the quarantine even if storage fails; callers stop on the error.
	if r.file != nil {
		err := json.NewEncoder(r.file).Encode(record{r.chain, e})
		if err == nil {
			err = r.file.Sync()
		}
		if err != nil {
			next.failure = fmt.Errorf("persist pool risk: %w", err)
		}
	}
	r.current.Store(next)
	return next.failure
}

func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.file != nil {
		err = r.file.Close()
		r.file = nil
	}
	if r.lock != nil {
		err = errors.Join(err, r.lock.Unlock())
		r.lock = nil
	}
	return err
}
