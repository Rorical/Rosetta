package remote

import (
	"math/big"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

// lenderRecentBlocks bounds the canonical blocks kept to roll a balance seed
// forward: the seed is read off the state writer, which keeps following.
const lenderRecentBlocks = 256

var transferTopic = digest([]byte("Transfer(address,address,uint256)"))

// lenderBook follows V3 flash lenders' balances of the reactive funding
// tokens. A hash-bound balanceOf read seeds them at a block, and the Transfer
// logs of each later canonical block move them, so an offer knows what a
// lender held at the last canonical block without a read per offer. Without
// it the cheapest WETH lender, holding a fraction of the loan, could be chosen
// and the preflight revert would quarantine the route's swap pools.
type lenderBook struct {
	mu      sync.Mutex
	tokens  map[domain.Address]bool
	lenders map[domain.Address]bool
	slots   []lenderSlot
	// balances describe block at; a zero at means unseeded.
	balances map[lenderSlot]*big.Int
	at       domain.BlockRef
	recent   []lenderMoves
}

type lenderSlot struct{ pool, token domain.Address }

type lenderMoves struct {
	ref   domain.BlockRef
	moves map[lenderSlot]*big.Int
}

// A nil book follows nothing and knows no balance.
func newLenderBook(tokens []domain.Address) *lenderBook {
	b := &lenderBook{tokens: map[domain.Address]bool{}, lenders: map[domain.Address]bool{}}
	for _, token := range tokens {
		if token != (domain.Address{}) {
			b.tokens[token] = true
		}
	}
	return b
}

// setLenders adopts the current V3 lender pools. A changed set is seeded
// again: a new lender has no seed to move.
func (b *lenderBook) setLenders(pools []pool.State) {
	if b == nil {
		return
	}
	var slots []lenderSlot
	for _, p := range pools {
		for _, token := range [2]domain.Address{p.Token0, p.Token1} {
			if b.tokens[token] {
				slots = append(slots, lenderSlot{p.Key.Address, token})
			}
		}
	}
	slices.SortFunc(slots, compareSlots)
	slots = slices.Compact(slots)
	b.mu.Lock()
	defer b.mu.Unlock()
	if slices.Equal(slots, b.slots) {
		return
	}
	b.slots = slots
	b.lenders = map[domain.Address]bool{}
	for _, slot := range slots {
		b.lenders[slot.pool] = true
	}
	b.balances, b.at = nil, domain.BlockRef{}
}

// observe records one canonical block's lender transfers and moves seeded
// balances along. A block that does not extend the seeded one unseeds them.
func (b *lenderBook) observe(ref domain.BlockRef, transactions []ingest.Transaction) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	moves := map[lenderSlot]*big.Int{}
	move := func(slot lenderSlot, amount *big.Int, sign int) {
		total := moves[slot]
		if total == nil {
			total = new(big.Int)
			moves[slot] = total
		}
		if sign > 0 {
			total.Add(total, amount)
		} else {
			total.Sub(total, amount)
		}
	}
	for _, tx := range transactions {
		for _, log := range tx.Logs {
			if !b.tokens[log.Address] || len(log.Topics) != 3 || log.Topics[0] != transferTopic || len(log.Data) != 32 {
				continue
			}
			from, to := topicAddress(log.Topics[1]), topicAddress(log.Topics[2])
			if !b.lenders[from] && !b.lenders[to] {
				continue
			}
			amount := new(big.Int).SetBytes(log.Data)
			if b.lenders[from] {
				move(lenderSlot{from, log.Address}, amount, -1)
			}
			if b.lenders[to] {
				move(lenderSlot{to, log.Address}, amount, 1)
			}
		}
	}
	b.recent = append(b.recent, lenderMoves{ref, moves})
	if len(b.recent) > lenderRecentBlocks {
		b.recent = slices.Delete(b.recent, 0, len(b.recent)-lenderRecentBlocks)
	}
	if b.at == (domain.BlockRef{}) {
		return
	}
	if ref.ParentHash != b.at.Hash {
		b.balances, b.at = nil, domain.BlockRef{}
		return
	}
	b.apply(moves)
	b.at = ref
}

func (b *lenderBook) apply(moves map[lenderSlot]*big.Int) {
	for slot, delta := range moves {
		if balance := b.balances[slot]; balance != nil {
			balance.Add(balance, delta)
		}
	}
}

// reset forgets balances and history after a reorg or replaced store.
func (b *lenderBook) reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.balances, b.at, b.recent = nil, domain.BlockRef{}, nil
}

// needsSeed returns the slots to read when the book is unseeded.
func (b *lenderBook) needsSeed() []lenderSlot {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at != (domain.BlockRef{}) || len(b.slots) == 0 {
		return nil
	}
	return slices.Clone(b.slots)
}

// install adopts balances read after block ref and rolls them forward through
// the blocks observed since. It reports false when ref is no longer in the
// observed history or the read covers another lender set.
func (b *lenderBook) install(ref domain.BlockRef, slots []lenderSlot, values []*big.Int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !slices.Equal(slots, b.slots) || len(values) != len(slots) {
		return false
	}
	start := slices.IndexFunc(b.recent, func(m lenderMoves) bool { return m.ref.Hash == ref.Hash })
	if start < 0 {
		return false
	}
	balances := make(map[lenderSlot]*big.Int, len(slots))
	for i, slot := range slots {
		balances[slot] = new(big.Int).Set(values[i])
	}
	b.balances, b.at = balances, ref
	for _, later := range b.recent[start+1:] {
		if later.ref.ParentHash != b.at.Hash {
			b.balances, b.at = nil, domain.BlockRef{}
			return false
		}
		b.apply(later.moves)
		b.at = later.ref
	}
	return true
}

// balance is a lender's balance of token at the last canonical block.
func (b *lenderBook) balance(lender domain.PoolKey, token domain.Address) (*big.Int, bool) {
	if b == nil {
		return nil, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	balance, ok := b.balances[lenderSlot{lender.Address, token}]
	if !ok {
		return nil, false
	}
	return new(big.Int).Set(balance), true
}

func compareSlots(a, b lenderSlot) int {
	if c := slices.Compare(a.pool[:], b.pool[:]); c != 0 {
		return c
	}
	return slices.Compare(a.token[:], b.token[:])
}

func topicAddress(topic domain.Hash) domain.Address {
	var a domain.Address
	copy(a[:], topic[12:])
	return a
}
