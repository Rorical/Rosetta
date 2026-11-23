package ingest

import (
	"context"
	"fmt"
	"slices"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

const (
	maxLogBlocks       = 128
	maxLogTransactions = 32768
	maxLogBytes        = 32 << 20
)

type logBlock struct {
	// Hash is a seal claimed by the next streamed block, never a verified root.
	ref          domain.BlockRef
	transactions []Transaction
}

// LogBranch maintains pool state across tentative block boundaries. Its owned
// receipt history permits canonical reconciliation without stalling ingestion.
// Only ResetTo/Reconcile install a canonical base; streamed seals remain claims.
// Drive it serially and pass View snapshots to concurrent quote workers.
type LogBranch struct {
	base                *state.Store
	working             *state.Store
	routes              routing
	affected            map[domain.PoolKey]struct{}
	blocks              []logBlock
	bytes, transactions int
	epoch               uint64
	version             domain.StateVersion
}

func NewLogBranch(base *state.Store) (*LogBranch, error) {
	b := &LogBranch{}
	if err := b.ResetTo(base); err != nil {
		return nil, err
	}
	return b, nil
}

// ResetTo abandons all tentative blocks and their quote versions.
func (b *LogBranch) ResetTo(base *state.Store) error {
	if base == nil || base.Block().Hash == (domain.Hash{}) {
		return fmt.Errorf("%w: log branch requires a verified base", ErrInvalidConfig)
	}
	var keys []domain.PoolKey
	for _, p := range base.Pools() {
		keys = append(keys, p.Key)
	}
	routes, err := newRouting(keys)
	if err != nil {
		return err
	}
	b.epoch++
	b.base, b.working, b.routes = base, nil, routes
	b.blocks = nil
	b.affected = make(map[domain.PoolKey]struct{})
	b.bytes, b.transactions = 0, 0
	b.version = domain.StateVersion{}
	return nil
}

// Begin starts the next streamed block, including an empty one. ParentHash is
// checked against the canonical anchor or the retained predecessor's seal.
// ref.Hash is cleared: a frame does not establish the current block's identity.
func (b *LogBranch) Begin(ctx context.Context, ref domain.BlockRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	previous := b.base.Block()
	if len(b.blocks) > 0 {
		previous = b.blocks[len(b.blocks)-1].ref
	}
	if previous.Number == ^uint64(0) || ref.Number != previous.Number+1 || ref.ParentHash == (domain.Hash{}) || previous.Hash != (domain.Hash{}) && ref.ParentHash != previous.Hash {
		return ErrFastContinuity
	}
	if len(b.blocks) >= maxLogBlocks {
		return ErrFastLimit
	}
	if b.working == nil {
		working := b.base.Fork()
		working.ClearDirty()
		b.working = working
	}
	ref.Hash = domain.Hash{}
	if err := b.working.BeginBlock(ref); err != nil {
		return err
	}
	b.working.Commit()
	b.working.ClearDirty()
	if len(b.blocks) > 0 {
		b.blocks[len(b.blocks)-1].ref.Hash = ref.ParentHash
	}
	b.blocks = append(b.blocks, logBlock{ref: ref})
	previousHash := b.version.PrefixHash
	if previousHash == (domain.Hash{}) {
		previousHash = b.base.Block().Hash
	}
	h := sha3.NewLegacyKeccak256()
	h.Write(previousHash[:])
	h.Write(ref.ParentHash[:])
	writeFastUint(h, ref.Number)
	writeFastUint(h, ref.Timestamp)
	var commitment domain.Hash
	copy(commitment[:], h.Sum(nil))
	b.version = domain.StateVersion{Source: domain.SourceFast, Anchor: b.base.Block(), Epoch: b.epoch, BlockNumber: ref.Number, PrefixHash: commitment}
	return nil
}

// Apply atomically applies one complete ordered receipt. The branch retains a
// private copy of its effects, so later canonical repair cannot depend on the
// transport's buffers or on pool snapshots taken from a different block.
func (b *LogBranch) Apply(ctx context.Context, tx Transaction) ([]domain.PoolKey, error) {
	if len(b.blocks) == 0 {
		return nil, ErrFastContinuity
	}
	block := &b.blocks[len(b.blocks)-1]
	if tx.Index != uint64(len(block.transactions)) || tx.Hash == (domain.Hash{}) {
		return nil, ErrFastContinuity
	}
	size := 256
	for _, log := range tx.Logs {
		size += 20 + 32*len(log.Topics) + len(log.Data)
	}
	if b.transactions >= maxLogTransactions || size > maxLogBytes-b.bytes {
		return nil, ErrFastLimit
	}
	ref := block.ref
	ref.Hash = domain.Hash{}
	dirty, err := applyTransaction(ctx, b.working, b.routes, ref, tx)
	if err != nil {
		return nil, err
	}
	for _, key := range dirty {
		b.affected[key] = struct{}{}
	}
	block.transactions = append(block.transactions, copyLogTransaction(tx))
	b.transactions++
	b.bytes += size
	b.version.TxIndex = tx.Index
	b.version.PrefixHash = extendFastPrefix(b.version.PrefixHash, fastPosition{ref.Number, tx.Index}, transactionDigest(tx))
	return dirty, nil
}

func (b *LogBranch) View() (state.View, domain.StateVersion, bool) {
	if b.working == nil {
		return state.View{}, domain.StateVersion{}, false
	}
	return b.working.View(), b.version, true
}

// AffectedPools identifies paths changed by the retained receipt suffix. A
// rebase retires search tickets, so these paths must be submitted again too.
func (b *LogBranch) AffectedPools() []domain.PoolKey {
	keys := make([]domain.PoolKey, 0, len(b.affected))
	for key := range b.affected {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return keys
}

func copyLogTransaction(tx Transaction) Transaction {
	tx.Logs = slices.Clone(tx.Logs)
	for i := range tx.Logs {
		tx.Logs[i].Topics = slices.Clone(tx.Logs[i].Topics)
		tx.Logs[i].Data = slices.Clone(tx.Logs[i].Data)
	}
	return tx
}
