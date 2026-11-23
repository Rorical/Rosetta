package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
)

// competitionObserver records transactions that change two or more tracked
// pools, seen in a flashblock or an Arbitrum Fast Feed message: rival
// arbitrage, plus some aggregator routes. Their bids are the data for estimating how much priority fee wins.
// Research data may be dropped; it never blocks the feed or stops sends.
type competitionObserver struct {
	records chan competitionSighting
	dropped atomic.Uint64
	signer  types.Signer
	// self holds every send lane's sender.
	self    map[domain.Address]bool
	onRival func(competitorRecord) // runs on the writer goroutine
	done    chan struct{}
}

// competitionSighting is what the main loop copies; decoding and sender
// recovery run on the writer goroutine.
type competitionSighting struct {
	raw          []byte
	hash         domain.Hash
	block, frame uint64
	index        uint64
	success      bool
	pools        []domain.PoolKey
	baseFee      *big.Int
	receivedAtNS int64
	// A canonical sighting has no raw transaction: its sender and paid tip
	// come from the receipt.
	from *domain.Address
	tip  *big.Int
}

type competitorRecord struct {
	Type         string           `json:"type"`
	Block        uint64           `json:"block"`
	Frame        uint64           `json:"frame"`
	Index        uint64           `json:"tx_index"`
	Hash         domain.Hash      `json:"hash"`
	From         domain.Address   `json:"from"`
	To           *domain.Address  `json:"to,omitempty"`
	Own          bool             `json:"own,omitempty"`
	Tip          *big.Int         `json:"tip_per_gas"`
	FeeCap       *big.Int         `json:"fee_cap"`
	BaseFee      *big.Int         `json:"base_fee"`
	GasLimit     uint64           `json:"gas_limit"`
	Success      bool             `json:"success"`
	Pools        []domain.PoolKey `json:"pools"`
	ReceivedAtNS int64            `json:"received_at_ns"`
}

const competitionQueue = 1024

// onRival, when set, receives every decoded transaction from another sender.
func openCompetitionObserver(ctx context.Context, path string, chainID uint64, senders []domain.Address, onRival func(competitorRecord)) (*competitionObserver, error) {
	self := map[domain.Address]bool{}
	for _, sender := range senders {
		self[sender] = true
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	o := &competitionObserver{records: make(chan competitionSighting, competitionQueue), signer: types.LatestSignerForChainID(new(big.Int).SetUint64(chainID)), self: self, onRival: onRival, done: make(chan struct{})}
	go o.run(ctx, file)
	return o, nil
}

// observeFlash records a flashblock transaction; its frame is the flashblock.
func (o *competitionObserver) observeFlash(block feed.Flashblock, tx feed.FlashTransaction, index uint64, changed []domain.PoolKey, baseFee *big.Int, received time.Time) {
	o.observe(tx.Raw, tx.Hash, block.Number, block.Index, index, tx.Successful, changed, baseFee, received)
}

// observeFast records an Arbitrum Fast Feed transaction. Its frame is the
// priority gas auction round, inside which tips order transactions.
func (o *competitionObserver) observeFast(message feed.FastMessage, index uint64, changed []domain.PoolKey, received time.Time) {
	o.observe(message.RawTx, message.TxHash, message.BlockNumber, message.PGARound, index, message.Receipt.Successful, changed, message.Receipt.BaseFee, received)
}

// observeCanonical records a canonical block transaction for a chain without
// a preconfirmation stream. There is no frame: the block is the round.
func (o *competitionObserver) observeCanonical(block uint64, tx ingest.Transaction, changed []domain.PoolKey, received time.Time) {
	if tx.From == nil {
		return
	}
	var baseFee *big.Int
	if tx.Fees != nil {
		baseFee = tx.Fees.BaseFee
	}
	o.enqueue(competitionSighting{hash: tx.Hash, block: block, index: tx.Index, success: tx.Successful, baseFee: baseFee, receivedAtNS: received.UnixNano(), from: tx.From, tip: tx.Fees.PriorityFee()}, changed)
}

// observe runs on the main loop and only copies what it already holds.
func (o *competitionObserver) observe(raw []byte, hash domain.Hash, block, frame, index uint64, success bool, changed []domain.PoolKey, baseFee *big.Int, received time.Time) {
	o.enqueue(competitionSighting{raw: raw, hash: hash, block: block, frame: frame, index: index, success: success, baseFee: baseFee, receivedAtNS: received.UnixNano()}, changed)
}

// competitionSingleTip is the tip per gas from which a transaction on a single
// pool of ours is written to the file: below it, it is a plain swap.
const competitionSingleTip = 1_000_000

// enqueue keeps sightings that change a pool of ours. It used to keep only those
// changing two or more: a rival whose route holds one of our pools was never
// seen, and about a third of reverts then had no recorded rival while the
// chain showed one ahead of us at a far higher tip.
func (o *competitionObserver) enqueue(sighting competitionSighting, changed []domain.PoolKey) {
	pools := slices.Clone(changed)
	slices.SortFunc(pools, domain.ComparePoolKeys)
	sighting.pools = slices.Compact(pools)
	if len(sighting.pools) == 0 {
		return
	}
	select {
	case o.records <- sighting:
	default:
		o.dropped.Add(1)
	}
}

func (o *competitionObserver) run(ctx context.Context, file *os.File) {
	defer close(o.done)
	defer file.Close()
	out := bufio.NewWriter(file)
	encoder := json.NewEncoder(out)
	write := func(s competitionSighting) {
		if record, ok := o.decode(s); ok {
			if o.onRival != nil && !record.Own {
				o.onRival(record)
			}
			if len(record.Pools) < 2 && !record.Own && (record.Tip == nil || record.Tip.Cmp(big.NewInt(competitionSingleTip)) < 0) {
				return
			}
			if err := encoder.Encode(record); err != nil {
				slog.Warn("competition record write failed", "error", err)
			}
		}
	}
	report := time.NewTicker(time.Minute)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			// Sightings already queued at shutdown are kept.
			for len(o.records) > 0 {
				write(<-o.records)
			}
			out.Flush()
			return
		case <-report.C:
			if n := o.dropped.Swap(0); n > 0 {
				encoder.Encode(map[string]any{"type": "competitor_dropped", "count": n, "at_ns": time.Now().UnixNano()})
			}
			out.Flush()
		case s := <-o.records:
			write(s)
			if len(o.records) == 0 {
				out.Flush()
			}
		}
	}
}

func (o *competitionObserver) decode(s competitionSighting) (competitorRecord, bool) {
	if s.raw == nil {
		if s.from == nil {
			return competitorRecord{}, false
		}
		return competitorRecord{Type: "competitor_tx", Block: s.block, Frame: s.frame, Index: s.index, Hash: s.hash, From: *s.from, Own: o.self[*s.from], Tip: s.tip, BaseFee: s.baseFee, Success: s.success, Pools: s.pools, ReceivedAtNS: s.receivedAtNS}, true
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(s.raw); err != nil {
		return competitorRecord{}, false // deposits and unknown envelopes carry no bid
	}
	sender, err := types.Sender(o.signer, &tx)
	if err != nil {
		return competitorRecord{}, false
	}
	record := competitorRecord{Type: "competitor_tx", Block: s.block, Frame: s.frame, Index: s.index, Hash: s.hash, From: domain.Address(sender), Own: o.self[domain.Address(sender)],
		FeeCap: tx.GasFeeCap(), BaseFee: s.baseFee, GasLimit: tx.Gas(), Success: s.success, Pools: s.pools, ReceivedAtNS: s.receivedAtNS}
	if to := tx.To(); to != nil {
		address := domain.Address(*to)
		record.To = &address
	}
	// The effective tip is what orders the transaction inside its flashblock
	// or auction round.
	record.Tip = new(big.Int).Set(tx.GasTipCap())
	if s.baseFee != nil {
		if room := new(big.Int).Sub(tx.GasFeeCap(), s.baseFee); room.Cmp(record.Tip) < 0 {
			record.Tip = room
		}
	}
	return record, true
}
