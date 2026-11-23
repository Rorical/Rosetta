package remote

import (
	"bytes"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
)

// immutableReads keeps reads that cannot change once observed, across blocks:
// the hash of deployed code (EIP-6780 leaves code in place after its creating
// transaction) and a pool's factory(), an immutable in every supported AMM.
// Per-snapshot caches re-read them every block; a Slipstream pool without a
// local fee model read both on every block, enough to push the follower past
// a hosted provider's request limit on a fast chain. Absent code is not
// kept, because an address can still be deployed to. Only snapshots with
// keepIdentity use it; executor checks keep re-reading on purpose.
type immutableReads struct {
	mu        sync.Mutex
	codes     map[immutableKey]domain.Hash
	factories map[immutableKey][]byte
}

type immutableKey struct {
	chain   uint64
	address domain.Address
}

// immutableLimit bounds each map; a full map starts over.
const immutableLimit = 1 << 16

var immutable immutableReads

var factorySelector = selector("factory()")

func (r *immutableReads) code(chain uint64, address domain.Address) (domain.Hash, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hash, ok := r.codes[immutableKey{chain, address}]
	return hash, ok
}

func (r *immutableReads) keepCode(chain uint64, address domain.Address, hash domain.Hash) {
	if hash == (domain.Hash{}) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.codes == nil || len(r.codes) >= immutableLimit {
		r.codes = make(map[immutableKey]domain.Hash)
	}
	r.codes[immutableKey{chain, address}] = hash
}

func (r *immutableReads) factory(chain uint64, pool domain.Address) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	output, ok := r.factories[immutableKey{chain, pool}]
	return output, ok
}

func (r *immutableReads) keepFactory(chain uint64, pool domain.Address, output []byte) {
	if len(output) != 32 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil || len(r.factories) >= immutableLimit {
		r.factories = make(map[immutableKey][]byte)
	}
	r.factories[immutableKey{chain, pool}] = bytes.Clone(output)
}
