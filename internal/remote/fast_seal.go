package remote

import (
	"math/big"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// Arbitrum One header constants: Nitro's block gas limit, the same on every
// block, and the sequencer coinbase of the sequencer's own messages, as
// eth_getBlockByNumber reports them.
var (
	nitroBlockGasLimit uint64 = 0x4000000000000
	nitroCoinbase             = domain.Address{0xa4, 0xb0, 11: 0x73, 0x65, 0x71, 0x75, 0x65, 0x6e, 0x63, 0x65, 0x72}
)

const (
	// fastSealHistory bounds the Fast blocks and standard messages kept while
	// waiting for their counterpart: 64 s of Arbitrum One blocks.
	fastSealHistory = 256
	// fastSealWait is how many standard blocks wait for a Fast stream that has
	// not reached them, one second, before they go out as hints.
	fastSealWait = 4
)

// fastSealer assembles Arbitrum One canonical blocks from two streams, so the
// follower can take them like OP flashblocks and verify a sample by RPC.
// The Fast Feed sends each transaction with its receipt; a block is complete
// once the next block's first transaction arrives. The standard feed names the
// block hash, and its header gives the timestamp and L1 block. Sequence numbers
// map to blocks through the offset feedArrivals learned from an RPC block.
//
// The Fast goroutine calls fast, the standard feed goroutine calls standard.
type fastSealer struct {
	mu       sync.Mutex
	out      chan<- flashSeal
	arrivals *feedArrivals
	// firstIndex is the Fast index of a block's first transaction;
	// canonicalFirst the receipt index it has.
	firstIndex, canonicalFirst uint64
	blocks                     map[uint64]*fastBlock
	queue                      []standardBlock // standard messages not yet sealed, by sequence
	last                       *standardBlock  // the standard message sealed last
	fastTip                    uint64          // the highest block the Fast stream started
}

type fastBlock struct {
	transactions []ingest.Transaction
	hashes       []domain.Hash
	baseFee      *big.Int
	next         uint64 // the Fast index expected next
	intact       bool
	complete     bool
}

type standardBlock struct {
	seq       uint64
	hash      domain.Hash
	timestamp uint64
	l1Block   uint64
	// coinbase is the message's poster: the sequencer for its own messages,
	// the L1 sender for a delayed message or a batch posting report.
	coinbase domain.Address
}

func newFastSealer(out chan<- flashSeal, arrivals *feedArrivals, firstIndex, canonicalFirst uint64) *fastSealer {
	return &fastSealer{out: out, arrivals: arrivals, firstIndex: firstIndex, canonicalFirst: canonicalFirst, blocks: make(map[uint64]*fastBlock)}
}

// reset forgets the Fast blocks after a Fast reconnect: the feed has no replay,
// so a block in progress may miss transactions.
func (s *fastSealer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.blocks)
}

func (s *fastSealer) fast(message feed.FastMessage) {
	if message.TxIndex < s.firstIndex {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	number := message.BlockNumber
	block := s.blocks[number]
	if message.TxIndex == s.firstIndex {
		if previous := s.blocks[number-1]; previous != nil {
			previous.complete = true
		}
		// A second first transaction replaces the block; trust neither.
		replaced := block != nil
		block = &fastBlock{next: s.firstIndex, intact: !replaced}
		s.blocks[number] = block
		s.fastTip = max(s.fastTip, number)
		delete(s.blocks, number-fastSealHistory)
	}
	if block == nil {
		return // joined mid-block
	}
	if message.TxIndex != block.next {
		block.intact = false
		return
	}
	block.next++
	block.baseFee = message.Receipt.BaseFee
	r := message.Receipt
	tx := ingest.Transaction{Hash: message.TxHash, Index: s.canonicalFirst + (message.TxIndex - s.firstIndex), Successful: r.Successful, Raw: message.RawTx}
	// Nitro's gas used includes the L1 poster gas, as in its RPC receipts.
	gas := new(big.Int).SetUint64(r.GasUsed)
	execution := new(big.Int).Mul(gas, r.EffectiveGasPrice)
	tx.Fees = &domain.ReceiptFees{BaseFee: r.BaseFee, Family: domain.FamilyNitro, GasUsed: r.GasUsed, GasPrice: r.EffectiveGasPrice, Execution: execution, Total: new(big.Int).Set(execution)}
	for _, log := range r.Logs {
		tx.Logs = append(tx.Logs, ingest.Log{Address: log.Address, Topics: log.Topics, Data: log.Data})
	}
	block.transactions = append(block.transactions, tx)
	block.hashes = append(block.hashes, message.TxHash)
	s.seal()
}

func (s *fastSealer) standard(message feed.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	block := standardBlock{seq: message.Sequence, hash: message.BlockHash, timestamp: message.Header.Timestamp, l1Block: message.Header.L1BlockNumber, coinbase: nitroCoinbase}
	if message.Header.Poster != nil {
		block.coinbase = *message.Header.Poster
	}
	s.queue = append(s.queue, block)
	if len(s.queue) > fastSealHistory {
		s.queue = s.queue[len(s.queue)-fastSealHistory:]
	}
	s.seal()
}

// seal emits the queued standard blocks, in order, whose Fast block is
// complete. A standard block whose Fast block is broken or unfinished, or
// whose parent message was not seen, goes out as a hint only: the follower
// reads it by RPC. A Fast stream that is behind gets fastSealWait blocks.
func (s *fastSealer) seal() {
	offset, ok := s.arrivals.sequenceOffset()
	if !ok {
		return
	}
	for len(s.queue) > 0 {
		head := s.queue[0]
		number := uint64(int64(head.seq) + offset)
		block := s.blocks[number]
		behind := block == nil && s.fastTip <= number || block != nil && !block.complete
		if behind && len(s.queue) <= fastSealWait {
			return
		}
		contiguous := s.last != nil && s.last.seq+1 == head.seq
		if contiguous {
			// ArbOS keeps both monotonic; RPC verification checks them.
			head.timestamp, head.l1Block = max(head.timestamp, s.last.timestamp), max(head.l1Block, s.last.l1Block)
		}
		var sealed *flashBlock
		if block != nil && block.complete && block.intact && contiguous {
			ref := domain.BlockRef{Number: number, Hash: head.hash, ParentHash: s.last.hash, Timestamp: head.timestamp, L1Block: head.l1Block}
			sealed = &flashBlock{block: rpc.ExecutionBlock{Ref: ref, BaseFee: block.baseFee, GasLimit: nitroBlockGasLimit, Coinbase: head.coinbase, Transactions: block.hashes}, transactions: block.transactions}
		}
		select {
		case s.out <- flashSeal{number: number, hash: head.hash, block: sealed}:
		default: // the follower reads a dropped block by RPC
		}
		delete(s.blocks, number)
		s.last = &head
		s.queue = s.queue[1:]
	}
}
