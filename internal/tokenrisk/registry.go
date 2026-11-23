// Package tokenrisk retains observed transfer incompatibilities. Absence of
// evidence is not a certification: contract balance guards remain mandatory.
package tokenrisk

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/gofrs/flock"
)

var ErrBlocked = errors.New("token transfer amount is incompatible with execution")

// Evidence sources: our executor's traced transfer, or a canonical receipt in
// which a pool's Swap sent more of the token than its recipient received.
const (
	SourceExecutorTrace   = "executor_trace"
	SourceReceiptTransfer = "receipt_transfer"
)

type Evidence struct {
	Token     domain.Address  `json:"token"`
	Requested *big.Int        `json:"requested"`
	Received  *big.Int        `json:"received"`
	Block     domain.BlockRef `json:"block"`
	Source    string          `json:"source"`
}
type record struct {
	Chain    domain.ChainIdentity `json:"chain"`
	Evidence Evidence             `json:"evidence"`
}
type riskSet struct {
	tokens  map[domain.Address]bool
	failure error
}

// Registry publishes immutable sets so search and pre-sign checks do not take
// the persistence lock. Evidence is sticky across reorgs: it records a token's
// demonstrated behavior, rather than claiming that its tax stays constant.
type Registry struct {
	mu      sync.Mutex
	chain   domain.ChainIdentity
	current atomic.Pointer[riskSet]
	file    *os.File
	lock    *flock.Flock
}

func Open(path string, chain domain.ChainIdentity) (*Registry, error) {
	if chain.ChainID == 0 || (chain.Family != domain.FamilyOPStack && chain.Family != domain.FamilyNitro) {
		return nil, fmt.Errorf("token risk registry requires a chain")
	}
	r := &Registry{chain: chain}
	set := &riskSet{tokens: make(map[domain.Address]bool)}
	r.current.Store(set)
	if path == "" {
		return r, nil
	}
	r.lock = flock.New(path + ".lock")
	ok, err := r.lock.TryLock()
	if err != nil || !ok {
		return nil, fmt.Errorf("token risk registry already owned or unavailable")
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
			err = fmt.Errorf("token risk registry chain mismatch")
			break
		}
		if err = validate(row.Evidence); err != nil {
			break
		}
		set.tokens[row.Evidence.Token] = true
	}
	if err == nil {
		err = scanner.Err()
	}
	if err != nil {
		r.Close()
		return nil, fmt.Errorf("load token risk registry: %w", err)
	}
	// Make a newly created journal's directory entry durable before allowing
	// evidence to be acknowledged by Record.
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
	if e.Token == (domain.Address{}) || e.Requested == nil || e.Received == nil || e.Requested.Sign() <= 0 || e.Received.Sign() < 0 || e.Received.Cmp(e.Requested) >= 0 || e.Requested.BitLen() > 256 || e.Block.Hash == (domain.Hash{}) || e.Source != SourceExecutorTrace && e.Source != SourceReceiptTransfer {
		return fmt.Errorf("invalid token transfer shortfall evidence")
	}
	return nil
}

func (r *Registry) Check(token domain.Address) error {
	if r == nil {
		return nil
	}
	set := r.current.Load()
	if set.failure != nil {
		return set.failure
	}
	if set.tokens[token] {
		return fmt.Errorf("%w: %s", ErrBlocked, token)
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
	if old.tokens[e.Token] {
		return nil
	}
	next := &riskSet{tokens: make(map[domain.Address]bool, len(old.tokens)+1)}
	for token := range old.tokens {
		next.tokens[token] = true
	}
	next.tokens[e.Token] = true
	// Publish the exclusion even if storage fails; callers stop on the error.
	if r.file != nil {
		err := json.NewEncoder(r.file).Encode(record{r.chain, e})
		if err == nil {
			err = r.file.Sync()
		}
		if err != nil {
			next.failure = fmt.Errorf("persist token risk: %w", err)
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
