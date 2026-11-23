package pipeline

import (
	"math/big"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

type quoteDirection struct {
	Pool    domain.PoolKey
	TokenIn domain.Address
}
type quoteSample struct {
	block    uint64
	pips     uint32
	tx       domain.Hash
	logIndex int
}
type paidSample struct {
	shape [17]byte
	block domain.BlockRef
	tx    domain.Hash
	fees  *domain.ReceiptFees
}
type paperBook struct {
	mu          sync.RWMutex
	policy      PaperCostPolicy
	head        domain.BlockRef
	headers     map[uint64]domain.BlockRef
	quotes      map[quoteDirection][]quoteSample
	paid        []paidSample
	conversions map[domain.Address]conversionPath
	pending     bool
	revision    uint64
}

func newPaperBook(policy PaperCostPolicy) *paperBook {
	return &paperBook{policy: policy, headers: map[uint64]domain.BlockRef{}, quotes: map[quoteDirection][]quoteSample{}}
}
func paperShape(keys []domain.PoolKey) (shape [17]byte) {
	shape[0] = byte(len(keys))
	for i, key := range keys {
		shape[i+1] = byte(key.Kind)
	}
	slices.Sort(shape[1 : 1+len(keys)])
	return
}
func (b *paperBook) observeReceipts(base poolstate.View, ref domain.BlockRef, transactions []ingest.Transaction) {
	b.mu.Lock()
	defer b.mu.Unlock()
	restored := b.pending
	// A restored book cannot price anything until a canonical observation
	// proves a retained block identity. Truncate samples beyond that point.
	if b.pending {
		if saved, ok := b.headers[ref.Number]; ok && saved.Hash == ref.Hash {
			b.truncate(ref.Number)
			b.head = ref
		} else if saved, ok := b.headers[ref.Number-1]; ref.Number > 0 && ok && saved.Hash == ref.ParentHash {
			b.truncate(ref.Number - 1)
			b.head = saved
		} else {
			b.clear()
		}
		b.pending = false
		b.revision++
	}
	if ref == b.head && !restored {
		return
	}
	pools := make([]pool.State, 0)
	for _, key := range base.PoolKeys() {
		p, _ := base.Pool(key)
		pools = append(pools, p)
	}
	b.prepareConversions(pools)
	if ref == b.head {
		return
	}
	if b.head.Hash != (domain.Hash{}) && ref.ParentHash != b.head.Hash {
		if parent, ok := b.headers[ref.Number-1]; ref.Number > 0 && ok && parent.Hash == ref.ParentHash {
			b.truncate(parent.Number)
		} else {
			b.clear()
		}
	}
	b.head = ref
	b.headers[ref.Number] = ref
	b.revision++
	b.prune()
	byAddress := map[domain.Address][]pool.State{}
	for _, p := range pools {
		byAddress[p.Key.Address] = append(byAddress[p.Key.Address], p)
	}
	for _, tx := range transactions {
		if !tx.Successful || tx.Fees == nil || tx.Fees.Total == nil || tx.Fees.Total.Sign() < 0 || tx.Hash == (domain.Hash{}) {
			continue
		}
		var keys []domain.PoolKey
		complete := true
		for _, log := range tx.Logs {
			if len(log.Topics) == 0 || !dex.IsSwapTopic(log.Topics[0]) {
				continue
			}
			var matched []domain.PoolKey
			for _, p := range byAddress[log.Address] {
				if p.Key.IsV4() && (len(log.Topics) < 2 || log.Topics[1] != p.Key.PoolID) {
					continue
				}
				matched = append(matched, p.Key)
			}
			if len(matched) != 1 {
				complete = false
				break
			}
			keys = append(keys, matched[0])
			if len(keys) > 16 {
				complete = false
				break
			}
		}
		if !complete || len(keys) < 1 {
			continue
		}
		b.paid = append(b.paid, paidSample{paperShape(keys), ref, tx.Hash, tx.Fees})
	}
	// Bounded independent of pool/transaction activity; owned receipt values are immutable.
	if len(b.paid) > 2048 {
		b.paid = slices.Clone(b.paid[len(b.paid)-2048:])
	}
}
func (b *paperBook) observeQuotes(ref domain.BlockRef, result SwapAuditResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if saved, ok := b.headers[ref.Number]; !ok || saved.Hash != ref.Hash {
		return
	}
	for _, row := range result.Comparisons {
		if row.Quoted == nil || row.Quoted.Sign() <= 0 || row.Observed == nil || row.Observed.Sign() <= 0 || row.Delta == nil {
			continue
		}
		pips := uint32(0)
		if row.Delta.Sign() > 0 && row.Explanation != "exact_output_rounding_match" {
			ratio := new(big.Int).Mul(row.Delta, big.NewInt(1000000))
			q, rem := new(big.Int), new(big.Int)
			q.QuoRem(ratio, row.Quoted, rem)
			if rem.Sign() > 0 {
				q.Add(q, big.NewInt(1))
			}
			if q.Cmp(big.NewInt(1000000)) > 0 {
				q.SetInt64(1000000)
			}
			pips = uint32(q.Uint64())
		}
		key := quoteDirection{row.Pool, row.TokenIn}
		samples := b.quotes[key]
		duplicate := false
		for _, sample := range samples {
			if sample.block == ref.Number && sample.tx == row.Transaction && sample.logIndex == row.LogIndex {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		samples = append(samples, quoteSample{ref.Number, pips, row.Transaction, row.LogIndex})
		b.revision++
		if len(samples) > 64 {
			samples = slices.Clone(samples[len(samples)-64:])
		}
		b.quotes[key] = samples
	}
	b.limitQuotes()
}
