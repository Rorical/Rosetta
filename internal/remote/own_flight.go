package remote

import (
	"slices"
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

const (
	ownFlightKeep = 16
	// ownUnseen is how long a sent transaction that the feed has not shown
	// may still land. Sends appear 100–262 ms after the call.
	ownUnseen = 2 * time.Second
	// ownRevertHold bounds a hold on the pools of an unexplained revert. The
	// canonical revert diagnosis records a short-paying token within a few
	// blocks; the token risk registry excludes it from then on.
	ownRevertHold = time.Minute
)

// ownFlight tracks the pools our sent transactions swap through until the
// quote view includes their effects. Feed frames apply our transactions'
// logs like any other, so a view at or past the position where one landed
// already reflects it. The gap is a transaction sent but not yet seen, and
// one seen past the view's position: a route through its pools was quoted on
// state it is about to change, and reverts behind our own earlier send on a
// shared pool.
//
// It also holds the pools of a revert that competition cannot explain: none
// of the route's pools changed after the quote, so the next frame would quote
// and send the same failing route again (a transfer-taxed token can draw
// several failing sends within a second).
type ownFlight struct {
	mu      sync.Mutex
	txs     []ownTx // oldest first
	changes map[domain.PoolKey]streamPosition
	holds   map[domain.PoolKey]ownHold
	// sending holds each lane's route and variant group while its send is in
	// progress, before sent can record it.
	sending map[int]ownSend
}

type ownSend struct {
	pools []domain.PoolKey
	group uint64
}

// streamPosition is a transaction's place in the stream.
type streamPosition struct{ block, index uint64 }

func (p streamPosition) after(o streamPosition) bool {
	return p.block > o.block || p.block == o.block && p.index > o.index
}

type ownHold struct {
	at    streamPosition
	since time.Time
	// limit ends the hold when the pool never changes; zero is ownRevertHold.
	limit time.Duration
}

// ownTx is one nonce of one send lane. group names the variants of
// one opportunity raced on several lanes: conditions void all but the
// first to execute, so they do not hold each other back.
type ownTx struct {
	lane         int
	group        uint64
	nonce        uint64
	pools        []domain.PoolKey
	sent         time.Time
	seen         bool
	block, index uint64
}

// sent records a signed variant's route. Variants of one nonce share an entry.
func (f *ownFlight) sent(lane int, nonce uint64, pools []domain.PoolKey, group uint64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx := f.entry(lane, nonce)
	tx.sent, tx.group = at, group
	for _, key := range pools {
		if !slices.Contains(tx.pools, key) {
			tx.pools = append(tx.pools, key)
		}
	}
}

// seen records where the feed showed a variant of nonce. It may arrive
// before sent returns.
func (f *ownFlight) seen(lane int, nonce, block, index uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tx := f.entry(lane, nonce)
	tx.seen, tx.block, tx.index = true, block, index
}

// changed records that the transaction at block and index changed pools.
func (f *ownFlight) changed(pools []domain.PoolKey, block, index uint64) {
	if len(pools) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.changes == nil {
		f.changes = make(map[domain.PoolKey]streamPosition)
	}
	for _, key := range pools {
		f.changes[key] = streamPosition{block, index}
	}
}

// reverted records our transaction that reverted at block and index on a
// route quoted at version. Its pools are held only when none changed after
// the quote; a change means a rival moved the price first.
func (f *ownFlight) reverted(pools []domain.PoolKey, version domain.StateVersion, block, index uint64, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range pools {
		if last, ok := f.changes[key]; ok && !includedIn(last.block, last.index, version) {
			return
		}
	}
	if f.holds == nil {
		f.holds = make(map[domain.PoolKey]ownHold)
	}
	for _, key := range pools {
		f.holds[key] = ownHold{at: streamPosition{block, index}, since: now}
	}
}

// held returns the pools of unexplained reverts that have not changed since.
// ownRejectHold bounds how long the pools of a rejected conditional send are
// held when the stream never shows them change. A condition fails because a
// pool moved before our position; that move reaches the Fast view within a
// block or two.
const ownRejectHold = 2 * time.Second

// rejected holds the pools of a conditional send the sequencer dropped: one
// of them changed and the view the next intent is built on may not show it
// yet. Each is held until the stream reports a change after the last one
// known now. A pool under a revert's longer hold keeps it.
func (f *ownFlight) rejected(pools []domain.PoolKey, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holds == nil {
		f.holds = make(map[domain.PoolKey]ownHold)
	}
	for _, key := range pools {
		if _, held := f.holds[key]; held {
			continue
		}
		f.holds[key] = ownHold{at: f.changes[key], since: now, limit: ownRejectHold}
	}
}

func (f *ownFlight) held(now time.Time) []domain.PoolKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	var pools []domain.PoolKey
	for key, hold := range f.holds {
		limit := hold.limit
		if limit == 0 {
			limit = ownRevertHold
		}
		if now.Sub(hold.since) >= limit || f.changes[key].after(hold.at) {
			delete(f.holds, key)
			continue
		}
		pools = append(pools, key)
	}
	return pools
}

// reserve marks lane as sending through pools and reports whether no other
// lane is sending through any of them, a variant of the same group aside.
func (f *ownFlight) reserve(lane int, pools []domain.PoolKey, group uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for other, send := range f.sending {
		if other != lane && overlaps(pools, send.pools) && (group == 0 || group != send.group) {
			return false
		}
	}
	if f.sending == nil {
		f.sending = map[int]ownSend{}
	}
	f.sending[lane] = ownSend{pools, group}
	return true
}

func (f *ownFlight) release(lane int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sending, lane)
}

func (f *ownFlight) entry(lane int, nonce uint64) *ownTx {
	for i := range f.txs {
		if f.txs[i].lane == lane && f.txs[i].nonce == nonce {
			return &f.txs[i]
		}
	}
	if len(f.txs) == ownFlightKeep {
		f.txs = slices.Delete(f.txs, 0, 1)
	}
	f.txs = append(f.txs, ownTx{lane: lane, nonce: nonce})
	return &f.txs[len(f.txs)-1]
}

// moved returns the pools our transactions may have changed beyond version.
// A transaction being replaced is left out: the replacement executes instead
// of it, or fails on its nonce at no cost. Only the primary lane replaces.
// A variant of group is left out too: one of them executes and voids the
// others.
func (f *ownFlight) moved(version domain.StateVersion, replaced *execution.PendingTransaction, group uint64, now time.Time) []domain.PoolKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	var pools []domain.PoolKey
	for _, tx := range f.txs {
		if replaced != nil && tx.lane == 0 && tx.nonce == replaced.Nonce || group != 0 && tx.group == group {
			continue
		}
		if tx.seen && !includedIn(tx.block, tx.index, version) || !tx.seen && now.Sub(tx.sent) < ownUnseen {
			pools = append(pools, tx.pools...)
		}
	}
	return pools
}

// includedIn reports whether a view at version contains the transaction at
// block and index. A Fast view has applied its block up to TxIndex; others
// whole blocks.
func includedIn(block, index uint64, version domain.StateVersion) bool {
	if block != version.BlockNumber {
		return block < version.BlockNumber
	}
	return version.Source != domain.SourceFast || index <= version.TxIndex
}

func routePools(intent execution.ReactiveIntent) []domain.PoolKey {
	pools := make([]domain.PoolKey, len(intent.Route.Steps))
	for i, step := range intent.Route.Steps {
		pools[i] = step.Pool
	}
	return pools
}
