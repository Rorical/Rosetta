package remote

import (
	"bytes"
	"fmt"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/rpc"
)

const (
	// flashVerifyEvery is the probation: a new follower, and one that found a
	// mismatch, checks this many stream blocks one by one before trusting the
	// stream. Once trusted it checks one block a minute (verifyEvery).
	flashVerifyEvery    = 30
	flashVerifyInterval = time.Minute
	// flashHeadCheck is the RPC head read that measures lag and finds a stuck
	// stream while the stream supplies every block.
	flashHeadCheck = 30 * time.Second
	// flashSealStale returns the follower to RPC head polling: three OP blocks
	// without a sealed block.
	flashSealStale = 6 * time.Second
	// flashRecentBlocks is how far back the follower compares a stale stream
	// seal or a lagging RPC head with the blocks it accepted.
	flashRecentBlocks = 256
	flashPendingSeals = 128
)

// flashBlock is one canonical block assembled from every flashblock frame of
// its payload. Receipts in the stream carry status and logs only, so the
// transactions have no fees.
type flashBlock struct {
	block        rpc.ExecutionBlock
	transactions []ingest.Transaction
	feeParams    opFeeParams // l1InfoFeeParams of the block's first transaction
}

// flashSeal reports that the stream started a block whose base names hash as
// its parent, so the block at number is sealed with that hash. block is nil
// when the stream did not deliver that block whole and intact; the seal is
// then only a hint that the head moved.
type flashSeal struct {
	number uint64
	hash   domain.Hash
	block  *flashBlock
}

// flashAssembler runs on the stream goroutine, after receipt completion. Its
// caller has already checked frame continuity within a payload and resets it
// on every discontinuity, contradiction and reconnect.
type flashAssembler struct {
	out     chan<- flashSeal
	current *flashBlock
	intact  bool
	started uint64 // highest block whose index-zero frame was seen
}

func (a *flashAssembler) reset() { a.current = nil }

// frame adds one frame. hydrated marks receipts that RPC completed: the block
// is then not taken from the stream alone.
func (a *flashAssembler) frame(block feed.Flashblock, hydrated bool) {
	if block.Index != 0 {
		if a.current != nil && a.current.block.Ref.Number == block.Number {
			a.append(block, hydrated)
		}
		return
	}
	if block.Number > 0 {
		seal := flashSeal{number: block.Number - 1, hash: block.Base.Parent}
		if c := a.current; c != nil && a.intact && c.block.Ref.Number+1 == block.Number {
			c.block.Ref.Hash = block.Base.Parent
			seal.block = c
		}
		// The follower drains this channel between RPC calls. A full channel
		// drops the seal; the follower then reads the missing block by RPC.
		select {
		case a.out <- seal:
		default:
		}
	}
	// A second index-zero frame for a height replaces its payload. Neither
	// version is trusted; RPC supplies that block.
	replaced := block.Number <= a.started
	a.started = max(a.started, block.Number)
	ref := domain.BlockRef{Number: block.Number, ParentHash: block.Base.Parent, Timestamp: block.Base.Timestamp}
	a.current = &flashBlock{block: rpc.ExecutionBlock{Ref: ref}}
	a.intact = !replaced && block.Base.Execution != nil
	if execution := block.Base.Execution; execution != nil {
		a.current.block.BaseFee, a.current.block.GasLimit, a.current.block.Coinbase = execution.BaseFee, execution.GasLimit, execution.Coinbase
	}
	if len(block.Transactions) > 0 {
		a.current.feeParams = l1InfoFeeParams(block.Transactions[0].Raw)
	}
	a.append(block, hydrated)
}

func (a *flashAssembler) append(block feed.Flashblock, hydrated bool) {
	if hydrated {
		a.intact = false
	}
	c := a.current
	for _, transaction := range block.Transactions {
		tx := ingest.Transaction{Hash: transaction.Hash, Index: uint64(len(c.transactions)), Successful: transaction.Successful}
		for _, log := range transaction.Logs {
			tx.Logs = append(tx.Logs, ingest.Log{Address: log.Address, Topics: log.Topics, Data: log.Data})
		}
		c.transactions = append(c.transactions, tx)
		c.block.Transactions = append(c.block.Transactions, transaction.Hash)
	}
}

// flashCanonical is the follower's view of the stream's sealed blocks. The
// follower goroutine owns it.
//
// Trust: a stream block is applied only when its parent is the block the
// follower accepted last, so the hash chain is continuous back to an RPC
// verified block. Its content is the stream's: a block whose last frames never
// arrived, or a sealed block that differs from its frames, passes until the
// next verification compares it with RPC receipts. Verification runs on every
// verifyEvery-th stream block, and on every block while the stream is
// on probation.
type flashCanonical struct {
	seals    <-chan flashSeal
	pending  []flashSeal // increasing numbers above the head
	recent   [flashRecentBlocks]domain.BlockRef
	conflict domain.Hash // a seal contradicting an accepted block
	hinted   uint64      // newest seal already offered to RPC as a hint
	// streamTip is the newest sealed block; rpcTip the newest RPC head read.
	streamTip, rpcTip   uint64
	lastSeal, lastCheck time.Time
	// probation counts blocks still verified one by one; unverified counts
	// stream blocks applied since the last verification.
	probation, unverified int
	verifyEvery           int
	// applied holds the unverified stream blocks as applied, so one differing
	// block can be checked against them instead of reseeding.
	applied []flashBlock
}

// newFlashCanonical checks one of every verifyEvery trusted stream blocks.
func newFlashCanonical(seals <-chan flashSeal, verifyEvery uint64) *flashCanonical {
	return &flashCanonical{seals: seals, probation: flashVerifyEvery, verifyEvery: int(max(verifyEvery, 1))}
}

// drain takes every queued seal without waiting.
func (f *flashCanonical) drain(head domain.BlockRef, now time.Time) {
	for {
		select {
		case seal := <-f.seals:
			f.add(seal, head, now)
		default:
			return
		}
	}
}

func (f *flashCanonical) add(seal flashSeal, head domain.BlockRef, now time.Time) {
	f.lastSeal = now
	f.streamTip = seal.number
	if seal.number <= head.Number {
		if known := f.recent[seal.number%flashRecentBlocks]; known.Hash != (domain.Hash{}) && known.Number == seal.number && known.Hash != seal.hash {
			f.conflict = seal.hash
		}
		return
	}
	// A lower or repeated height means the stream rebuilt its chain.
	for len(f.pending) > 0 && f.pending[len(f.pending)-1].number >= seal.number {
		f.pending = f.pending[:len(f.pending)-1]
	}
	f.pending = append(f.pending, seal)
	if len(f.pending) > flashPendingSeals {
		f.pending = slices.Delete(f.pending, 0, len(f.pending)-flashPendingSeals)
	}
}

// ready reports whether the stream holds the block that extends head.
func (f *flashCanonical) ready(head domain.BlockRef) bool {
	for len(f.pending) > 0 && f.pending[0].number <= head.Number {
		f.pending = f.pending[1:]
	}
	if len(f.pending) == 0 || f.pending[0].number != head.Number+1 {
		return false
	}
	block := f.pending[0].block
	return block != nil && block.block.Ref.ParentHash == head.Hash
}

// next takes the stream block that extends head, if the stream has one.
func (f *flashCanonical) next(head domain.BlockRef) *flashBlock {
	if !f.ready(head) {
		return nil
	}
	block := f.pending[0].block
	f.pending = f.pending[1:]
	return block
}

// idle reports that the stream is live and has nothing past head, and the
// periodic RPC head check is not due: the follower waits for the next seal.
func (f *flashCanonical) idle(head domain.BlockRef, now time.Time) bool {
	return f.conflict == (domain.Hash{}) && f.streamTip <= head.Number && now.Sub(f.lastSeal) < flashSealStale && now.Sub(f.lastCheck) < flashHeadCheck
}

// wake is how long an idle follower may wait for a seal.
func (f *flashCanonical) wake(now time.Time) time.Duration {
	return max(min(f.lastCheck.Add(flashHeadCheck).Sub(now), f.lastSeal.Add(flashSealStale).Sub(now)), 0)
}

// hint names a block for the RPC path: a contradicting seal first, else the
// stream's newest sealed block, each offered once.
func (f *flashCanonical) hint(head domain.BlockRef) domain.Hash {
	if conflict := f.conflict; conflict != (domain.Hash{}) {
		f.conflict = domain.Hash{}
		return conflict
	}
	if len(f.pending) == 0 {
		return domain.Hash{}
	}
	newest := f.pending[len(f.pending)-1]
	if newest.number <= max(head.Number, f.hinted) {
		return domain.Hash{}
	}
	f.hinted = newest.number
	return newest.hash
}

// checked records an RPC head read.
func (f *flashCanonical) checked(tip uint64, now time.Time) {
	f.rpcTip, f.lastCheck = tip, now
}

func (f *flashCanonical) accepted(ref domain.BlockRef) {
	f.recent[ref.Number%flashRecentBlocks] = ref
}

// behind reports that an RPC head below head is one of the follower's own
// blocks: the RPC lags the stream, it did not reorganize.
func (f *flashCanonical) behind(tip, head domain.BlockRef) bool {
	known := f.recent[tip.Number%flashRecentBlocks]
	return tip.Number < head.Number && known.Number == tip.Number && known.Hash == tip.Hash
}

// reset forgets accepted blocks after a reseed.
func (f *flashCanonical) reset() {
	f.recent = [flashRecentBlocks]domain.BlockRef{}
	f.pending = nil
	f.applied = nil
	f.unverified = 0
}

func (f *flashCanonical) verifyDue() bool {
	return f.probation > 0 || f.unverified+1 >= f.verifyEvery
}

// apply counts a stream block applied without verification.
func (f *flashCanonical) apply(block flashBlock) {
	f.unverified++
	if len(f.applied) < flashRecentBlocks {
		f.applied = append(f.applied, block)
	}
}

// takeApplied returns the unverified blocks when all of them were kept.
func (f *flashCanonical) takeApplied() ([]flashBlock, bool) {
	applied := f.applied
	f.applied = nil
	return applied, len(applied) == f.unverified
}

func (f *flashCanonical) verified() {
	f.unverified = 0
	f.applied = nil
	f.probation = max(f.probation-1, 0)
}

// distrust puts the stream on probation after a mismatch.
func (f *flashCanonical) distrust() {
	f.probation = flashVerifyEvery
	f.pending = nil
}

func (f *flashCanonical) tip() uint64 { return max(f.streamTip, f.rpcTip) }

// sameStreamBlock reports whether a stream block has the RPC header's
// identity, execution context and transaction order.
func sameStreamBlock(stream, header rpc.ExecutionBlock) bool {
	return stream.Ref == header.Ref && stream.GasLimit == header.GasLimit && stream.Coinbase == header.Coinbase &&
		stream.BaseFee != nil && header.BaseFee != nil && stream.BaseFee.Cmp(header.BaseFee) == 0 &&
		slices.Equal(stream.Transactions, header.Transactions)
}

// streamDifference names the first field in which a stream block differs
// from the RPC block, for the mismatch log.
func streamDifference(stream, header rpc.ExecutionBlock, transactions []ingest.Transaction, receipts []rpc.ExecutionReceipt) string {
	switch {
	case stream.Ref.Hash != header.Ref.Hash:
		return "hash"
	case stream.Ref.Timestamp != header.Ref.Timestamp:
		return fmt.Sprintf("timestamp %d, RPC %d", stream.Ref.Timestamp, header.Ref.Timestamp)
	case stream.Ref.L1Block != header.Ref.L1Block:
		return fmt.Sprintf("L1 block %d, RPC %d", stream.Ref.L1Block, header.Ref.L1Block)
	case stream.Ref != header.Ref:
		return "number or parent hash"
	case stream.Coinbase != header.Coinbase:
		return fmt.Sprintf("coinbase %s, RPC %s", stream.Coinbase.Hex(), header.Coinbase.Hex())
	case stream.GasLimit != header.GasLimit:
		return fmt.Sprintf("gas limit %d, RPC %d", stream.GasLimit, header.GasLimit)
	case stream.BaseFee == nil || header.BaseFee == nil || stream.BaseFee.Cmp(header.BaseFee) != 0:
		return fmt.Sprintf("base fee %v, RPC %v", stream.BaseFee, header.BaseFee)
	case !slices.Equal(stream.Transactions, header.Transactions):
		return fmt.Sprintf("transaction list (%d, RPC %d)", len(stream.Transactions), len(header.Transactions))
	case len(transactions) != len(receipts):
		return fmt.Sprintf("%d transactions, RPC %d", len(transactions), len(receipts))
	}
	for i, tx := range transactions {
		if !sameTransaction(tx, uint64(i), receipts[i]) {
			return fmt.Sprintf("transaction %d", i)
		}
	}
	return ""
}

// sameEffects reports whether stream transactions have the receipts' status
// and logs. Fees exist only in the receipts.
func sameEffects(transactions []ingest.Transaction, receipts []rpc.ExecutionReceipt) bool {
	if len(transactions) != len(receipts) {
		return false
	}
	for i, tx := range transactions {
		if !sameTransaction(tx, uint64(i), receipts[i]) {
			return false
		}
	}
	return true
}

func sameTransaction(tx ingest.Transaction, index uint64, receipt rpc.ExecutionReceipt) bool {
	if tx.Hash != receipt.Hash || tx.Index != index || tx.Successful != receipt.Successful || len(tx.Logs) != len(receipt.Logs) {
		return false
	}
	for j, log := range tx.Logs {
		other := receipt.Logs[j]
		if log.Address != other.Address || !slices.Equal(log.Topics, other.Topics) || !bytes.Equal(log.Data, other.Data) {
			return false
		}
	}
	return true
}

// receiptTransactions converts verified block receipts in block order.
func receiptTransactions(receipts []rpc.ExecutionReceipt) []ingest.Transaction {
	transactions := make([]ingest.Transaction, len(receipts))
	for i, receipt := range receipts {
		tx := ingest.Transaction{Fees: receipt.Fees, Hash: receipt.Hash, From: receipt.From, Index: uint64(i), Successful: receipt.Successful}
		for _, log := range receipt.Logs {
			tx.Logs = append(tx.Logs, ingest.Log{Address: log.Address, Topics: log.Topics, Data: log.Data})
		}
		transactions[i] = tx
	}
	return transactions
}
