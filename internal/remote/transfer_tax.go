package remote

import (
	"log/slog"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

// receiptTaxSource names transfer-tax evidence read from canonical receipts.
const receiptTaxSource = tokenrisk.SourceReceiptTransfer

// transferShortfalls finds tokens that deliver less than a tracked pool sends.
// A V2, V3-style or Velodrome pool's Swap names its recipient and the amount
// it sent; when the same transaction's Transfers of that token from the pool
// to the recipient are all smaller, the token kept the difference. Only the
// token's own logs show this, so a token can incriminate only itself. Without
// it a taxed token is found only by the executor's preflight, which routes
// through it never reach, and such a token (E280 keeps 4% per transfer) can
// crowd a large share of the search candidates.
func transferShortfalls(ref domain.BlockRef, transactions []ingest.Transaction, pools map[domain.Address]pool.State, protected map[domain.Address]bool) []tokenrisk.Evidence {
	var found []tokenrisk.Evidence
	recorded := map[domain.Address]bool{}
	for _, tx := range transactions {
		if !tx.Successful {
			continue
		}
		for _, log := range tx.Logs {
			p, ok := pools[log.Address]
			if !ok || p.Key.Kind == domain.PoolKindV4 || len(log.Topics) < 3 {
				continue
			}
			_, sent, zeroForOne, swap := dex.DecodeSwapAmounts(p.Key.Kind, log.Topics, log.Data)
			if !swap || sent == nil || sent.Sign() <= 0 {
				continue
			}
			token := p.Token0
			if zeroForOne {
				token = p.Token1
			}
			if protected[token] || recorded[token] {
				continue
			}
			received, ok := deliveredTo(tx.Logs, token, log.Address, topicAddress(log.Topics[2]), sent)
			if !ok || received.Cmp(sent) >= 0 {
				continue
			}
			recorded[token] = true
			found = append(found, tokenrisk.Evidence{Token: token, Requested: new(big.Int).Set(sent), Received: received, Block: ref, Source: receiptTaxSource})
		}
	}
	return found
}

// deliveredTo is the largest Transfer of token from pool to recipient in one
// transaction, or exactly sent when any Transfer carried it; ok is false
// when no such Transfer was logged.
func deliveredTo(logs []ingest.Log, token, from, recipient domain.Address, sent *big.Int) (*big.Int, bool) {
	var largest *big.Int
	for _, log := range logs {
		if log.Address != token || len(log.Topics) != 3 || log.Topics[0] != transferTopic || len(log.Data) != 32 {
			continue
		}
		if topicAddress(log.Topics[1]) != from || topicAddress(log.Topics[2]) != recipient {
			continue
		}
		amount := new(big.Int).SetBytes(log.Data)
		if amount.Cmp(sent) == 0 {
			return amount, true
		}
		if largest == nil || amount.Cmp(largest) > 0 {
			largest = amount
		}
	}
	return largest, largest != nil
}

// recordTransferTaxes excludes the tokens a canonical block shows keeping part
// of a pool's transfer. Ranking reads the registry, so their routes leave the
// search at once; the next topology build drops their pools.
func (r *runtime) recordTransferTaxes(ref domain.BlockRef, transactions []ingest.Transaction) {
	if r.risks == nil {
		return
	}
	for _, e := range transferShortfalls(ref, transactions, r.swapPools(), r.protectedTokens()) {
		if r.risks.Check(e.Token) != nil {
			continue
		}
		if err := r.risks.Record(e); err != nil {
			slog.Warn("transfer tax not recorded", "token", e.Token.Hex(), "error", err)
			continue
		}
		slog.Warn("token keeps part of pool transfers; excluded", "token", e.Token.Hex(), "sent", e.Requested, "received", e.Received, "block", ref.Number)
	}
}

// swapPools maps each non-V4 pool address to its state, rebuilt only when the
// pool set changes.
func (r *runtime) swapPools() map[domain.Address]pool.State {
	var graph uint64
	if r.search != nil {
		graph = r.search.GraphVersion
	}
	if r.taxPools.pools != nil && r.taxPools.graph == graph && r.taxPools.count == r.store.Len() {
		return r.taxPools.pools
	}
	pools := map[domain.Address]pool.State{}
	for _, p := range r.store.Pools() {
		// A V4 pool shares its manager's address, and a wrap's address is
		// the wrapped native token, which is no swap pool.
		if p.Key.Kind != domain.PoolKindV4 && p.Key.Kind != domain.PoolKindWrap {
			pools[p.Key.Address] = p
		}
	}
	r.taxPools.pools, r.taxPools.graph, r.taxPools.count = pools, graph, r.store.Len()
	return pools
}

// protectedTokens are never excluded on receipt evidence: the search's
// funding assets, and the reactive assets and wrapped native.
func (r *runtime) protectedTokens() map[domain.Address]bool {
	protected := map[domain.Address]bool{}
	for token := range r.policy.Funding {
		protected[token] = true
	}
	if r.reactive != nil {
		protected[r.reactive.settings.wrapped] = true
		for token := range r.reactive.settings.minimum {
			protected[token] = true
		}
	}
	return protected
}
