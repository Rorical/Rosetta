package remote

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

func transferLog(token, from, to domain.Address, amount int64) ingest.Log {
	var fromTopic, toTopic domain.Hash
	copy(fromTopic[12:], from[:])
	copy(toTopic[12:], to[:])
	data := make([]byte, 32)
	big.NewInt(amount).FillBytes(data)
	return ingest.Log{Address: token, Topics: []domain.Hash{transferTopic, fromTopic, toTopic}, Data: data}
}

// A seed read after one block is rolled forward through the blocks observed
// since, and later Transfer logs keep moving it; a fork or a changed lender
// set unseeds the book.
func TestLenderBookFollowsSeededBalances(t *testing.T) {
	weth, usdc, other := domain.Address{0xe1}, domain.Address{0xe2}, domain.Address{0xe3}
	lender, trader := domain.Address{0x0a}, domain.Address{0x0b}
	book := newLenderBook([]domain.Address{weth, usdc, {}})
	pools := []pool.State{{Key: domain.PoolKey{Address: lender, Kind: domain.PoolKindV3}, Token0: weth, Token1: other}}
	book.setLenders(pools)
	block := func(n byte, parent domain.BlockRef) domain.BlockRef {
		return domain.BlockRef{Number: uint64(n), Hash: domain.Hash{n}, ParentHash: parent.Hash}
	}
	key := pools[0].Key
	b1 := block(1, domain.BlockRef{})
	b2, b3 := block(2, b1), block(3, block(2, b1))
	book.observe(b1, nil)
	if _, ok := book.balance(key, weth); ok {
		t.Fatal("an unseeded book reported a balance")
	}
	slots := book.needsSeed()
	if len(slots) != 1 || slots[0] != (lenderSlot{lender, weth}) {
		t.Fatal("seed slots", slots)
	}
	// Two blocks arrive while the seed read at block 1 is in flight.
	book.observe(b2, []ingest.Transaction{{Logs: []ingest.Log{transferLog(weth, lender, trader, 30), transferLog(usdc, trader, lender, 5)}}})
	book.observe(b3, []ingest.Transaction{{Logs: []ingest.Log{transferLog(weth, trader, lender, 7)}}})
	if !book.install(b1, slots, []*big.Int{big.NewInt(100)}) {
		t.Fatal("seed not installed")
	}
	if balance, ok := book.balance(key, weth); !ok || balance.Int64() != 77 {
		t.Fatal("rolled-forward balance", balance, ok)
	}
	b4 := block(4, b3)
	book.observe(b4, []ingest.Transaction{{Logs: []ingest.Log{transferLog(weth, lender, trader, 50), transferLog(other, lender, trader, 1)}}})
	if balance, _ := book.balance(key, weth); balance.Int64() != 27 {
		t.Fatal("followed balance", balance)
	}
	if book.needsSeed() != nil {
		t.Fatal("a seeded book asked for a seed")
	}
	// A block on another fork unseeds the book.
	book.observe(block(5, block(9, domain.BlockRef{})), nil)
	if _, ok := book.balance(key, weth); ok || book.needsSeed() == nil {
		t.Fatal("a fork kept stale balances")
	}
	// A seed for another lender set, or from outside the history, is refused.
	if book.install(block(42, domain.BlockRef{}), slots, []*big.Int{big.NewInt(1)}) {
		t.Fatal("a seed outside the observed history was installed")
	}
	book.setLenders(append(pools, pool.State{Key: domain.PoolKey{Address: domain.Address{0x0c}, Kind: domain.PoolKindV3}, Token0: usdc, Token1: other}))
	if book.install(b4, slots, []*big.Int{big.NewInt(1)}) {
		t.Fatal("a seed for the old lender set was installed")
	}
}
