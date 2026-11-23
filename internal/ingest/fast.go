package ingest

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/state"
)

var (
	ErrFastContinuity = errors.New("ingest: fast prefix is not continuous with its anchor")
	ErrFastConflict   = errors.New("ingest: fast receipt conflicts with observed execution")
	ErrFastLimit      = errors.New("ingest: fast prefix cache limit reached")
)

// FastOptions requires an explicit mapping of the first feed transaction to
// its standard receipt index. The Fast Feed schema does not establish whether
// system transactions are included. Validate this mapping against live data.
type FastOptions struct {
	FirstTxIndex          uint64
	CanonicalFirstTxIndex uint64
	MaxTransactions       int
	MaxBytes              int
}

type fastPosition struct{ block, index uint64 }

type fastRecord struct {
	message  feed.FastMessage
	digest   domain.Hash
	identity domain.Hash
}

// FastBranch owns a pool-only overlay and a bounded receipt prefix.
// It never owns or mutates execution state. Use it on one writer goroutine and
// capture View for concurrent readers. Every reset/rebase changes its epoch.
type FastBranch struct {
	base     *state.Store
	working  *state.Store
	routes   routing
	opts     FastOptions
	epoch    uint64
	version  domain.StateVersion
	records  []fastRecord
	seen     map[fastPosition]int
	bytes    int
	versions map[domain.PoolKey]uint64
	// waiting holds, while the branch is empty, a continuous run of messages
	// that starts at a block's first transaction past the base's child. The
	// canonical base can trail the stream by a block (Arbitrum One assembles
	// a block when the next one starts); Reconcile replays the run once
	// the base reaches its parent, instead of refusing every message.
	waiting []feed.FastMessage
}

func NewFastBranch(base *state.Store, opts FastOptions) (*FastBranch, error) {
	if opts.MaxTransactions < 0 || opts.MaxBytes < 0 {
		return nil, fmt.Errorf("%w: negative fast cache bounds", ErrInvalidConfig)
	}
	if opts.MaxTransactions == 0 {
		opts.MaxTransactions = 4096
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 32 << 20
	}
	b := &FastBranch{opts: opts}
	if err := b.ResetTo(base); err != nil {
		return nil, err
	}
	return b, nil
}

// ResetTo replaces the verified standard baseline after recovery or a topology
// switch. Even a rejected baseline invalidates old receipts and versions.
func (b *FastBranch) ResetTo(base *state.Store) error {
	b.Reset()
	if base == nil || base.Block().Hash == (domain.Hash{}) {
		return fmt.Errorf("%w: fast branch requires a verified base", ErrInvalidConfig)
	}
	copy := base.Fork()
	keys := make([]domain.PoolKey, 0)
	for _, p := range copy.Pools() {
		keys = append(keys, p.Key)
	}
	routes, err := newRouting(keys)
	if err != nil {
		return err
	}
	b.base, b.routes = copy, routes
	return nil
}

// Reset invalidates the prefix immediately. Recovery requires the next block's
// first transaction; joining at an arbitrary mid-block position is refused.
func (b *FastBranch) Reset() {
	b.epoch++
	b.working, b.records = nil, nil
	b.bytes = 0
	b.seen = make(map[fastPosition]int)
	b.versions = make(map[domain.PoolKey]uint64)
	b.version = domain.StateVersion{}
	b.waiting = nil
}

// Apply atomically reduces one complete receipt and advances its execution
// prefix even if the transaction reverted or touched no tracked pools. Any
// gap, conflicting duplicate or decode failure invalidates all old snapshots.
func (b *FastBranch) Apply(ctx context.Context, message feed.FastMessage) ([]domain.PoolKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx := fastTransaction(message)
	digest := transactionDigest(tx)
	identity := fastReceiptDigest(message, digest)
	position := fastPosition{message.BlockNumber, message.TxIndex}
	if index, ok := b.seen[position]; ok {
		if b.records[index].identity == identity {
			return nil, nil
		}
		b.Reset()
		return nil, ErrFastConflict
	}
	if tx.Hash != (domain.Hash{}) && len(message.RawTx) != 0 && len(b.records) == 0 && !b.follows(message) && b.wait(message) {
		return nil, nil
	}
	if !b.follows(message) || tx.Hash == (domain.Hash{}) || len(message.RawTx) == 0 {
		b.Reset()
		return nil, ErrFastContinuity
	}
	size := fastMessageBytes(message)
	if len(b.records) >= b.opts.MaxTransactions || size > b.opts.MaxBytes-b.bytes {
		b.Reset()
		return nil, ErrFastLimit
	}
	if b.working == nil {
		b.working = b.base.Fork()
		b.working.ClearDirty()
	}
	// No final Fast block hash exists. Do not store the prefix digest in a
	// BlockRef.Hash, where consumers would interpret it as a chain block hash.
	// Nor a header time: timestamp_ms is when the feed wrote the message. The
	// parent's time is a lower bound ArbOS keeps, and Arbitrum One blocks are
	// 250 ms apart; Slipstream oracles read it.
	ref := domain.BlockRef{Number: message.BlockNumber, Timestamp: b.working.Block().Timestamp}
	dirty, err := applyTransaction(ctx, b.working, b.routes, ref, tx)
	if err != nil {
		b.Reset()
		return nil, err
	}
	previous := b.version.PrefixHash
	if len(b.records) == 0 {
		previous = b.base.Block().Hash
	}
	b.version = domain.StateVersion{
		Source: domain.SourceFast, Anchor: b.base.Block(), Epoch: b.epoch,
		BlockNumber: message.BlockNumber, TxIndex: message.TxIndex,
		PrefixHash: extendFastPrefix(previous, position, identity),
	}
	for _, key := range dirty {
		b.versions[key]++
	}
	b.seen[position] = len(b.records)
	b.records = append(b.records, fastRecord{message: copyFastMessage(message), digest: digest, identity: identity})
	b.bytes += size
	return dirty, nil
}

// wait keeps a message of an empty branch that is ahead of the base's child
// and continues the waiting run, or starts one at a block's first
// transaction. It reports false for a message that can never apply.
func (b *FastBranch) wait(message feed.FastMessage) bool {
	if message.BlockNumber <= b.base.Block().Number+1 || len(b.waiting) >= b.opts.MaxTransactions {
		b.waiting = nil
		return false
	}
	if n := len(b.waiting); n > 0 && continues(b.waiting[n-1], message, b.opts.FirstTxIndex) {
		b.waiting = append(b.waiting, copyFastMessage(message))
		return true
	}
	if message.TxIndex != b.opts.FirstTxIndex {
		b.waiting = nil
		return false
	}
	b.waiting = []feed.FastMessage{copyFastMessage(message)}
	return true
}

// continues reports whether next is the transaction right after last.
func continues(last, next feed.FastMessage, firstIndex uint64) bool {
	return (next.BlockNumber == last.BlockNumber && next.TxIndex > last.TxIndex && next.TxIndex-last.TxIndex == 1) ||
		(next.BlockNumber > last.BlockNumber && next.BlockNumber-last.BlockNumber == 1 && next.TxIndex == firstIndex)
}

func (b *FastBranch) follows(message feed.FastMessage) bool {
	if len(b.records) == 0 {
		head := b.base.Block().Number
		return message.BlockNumber > head && message.BlockNumber-head == 1 && message.TxIndex == b.opts.FirstTxIndex
	}
	return continues(b.records[len(b.records)-1].message, message, b.opts.FirstTxIndex)
}

// View returns an immutable search snapshot and dependency counters. Captures
// from an earlier prefix remain readable but Current rejects them after advance.
func (b *FastBranch) View() (state.View, domain.StateVersion, map[domain.PoolKey]uint64, bool) {
	if b.working == nil {
		return state.View{}, domain.StateVersion{}, nil, false
	}
	return b.working.View(), b.version, maps.Clone(b.versions), true
}

func (b *FastBranch) Current(version domain.StateVersion) bool {
	return b.working != nil && b.version == version
}

func (b *FastBranch) Version() (domain.StateVersion, bool) {
	return b.version, b.working != nil
}
