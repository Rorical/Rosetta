package discovery

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestActivityCountsSwapsOnceAcrossReplayAndPrunesWindow(t *testing.T) {
	book := ActivityBook{}
	key := domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindV3}
	ref := domain.BlockRef{Number: 10, Hash: domain.Hash{10}}
	logs := []rpc.Log{{Address: key.Address, Topics: []domain.Hash{swapV3}}, {Address: key.Address, Topics: []domain.Hash{eventHash("Sync(uint112,uint112)")}}}
	book.Observe(ref, logs, []domain.PoolKey{key}, 2)
	book.Observe(ref, logs, []domain.PoolKey{key}, 2)
	if book.Snapshot()[key].Swaps != 1 {
		t.Fatal(book.Snapshot())
	}
	records := book.Records()
	restored := ActivityBook{}
	if err := restored.Restore(records, ref); err != nil {
		t.Fatal(err)
	}
	restored.Observe(ref, logs, []domain.PoolKey{key}, 2)
	if restored.Snapshot()[key].Swaps != 1 {
		t.Fatal("restart doubled activity")
	}
	ref.Number = 13
	ref.Hash = domain.Hash{13}
	restored.Observe(ref, nil, []domain.PoolKey{key}, 2)
	if restored.Snapshot()[key].Swaps != 0 {
		t.Fatal("old activity never expired")
	}
}
func TestActivityKeepsUnknownV4IDsSeparate(t *testing.T) {
	manager := domain.Address{1}
	book := ActivityBook{}
	a, b := domain.Hash{1}, domain.Hash{2}
	book.Observe(domain.BlockRef{Number: 1, Hash: domain.Hash{1}}, []rpc.Log{{Address: manager, Topics: []domain.Hash{swapV4, a}}, {Address: manager, Topics: []domain.Hash{swapV4, b}}}, nil, 100)
	if len(book.Unknown()) != 2 {
		t.Fatal("singleton pools collapsed", book.Unknown())
	}
}

func TestActivityBinsBoundMemoryAndMigrateUnknownCounts(t *testing.T) {
	book := ActivityBook{}
	key := domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindV3}
	logs := []rpc.Log{{Address: key.Address, Topics: []domain.Hash{swapV3}}}
	for number := uint64(1); number <= 10000; number++ {
		book.Observe(domain.BlockRef{Number: number, Hash: domain.Hash{1}}, logs, nil, 7200)
	}
	if len(book.Records()) != 1 || len(book.Records()[0].Counts) > 129 {
		t.Fatal("unbounded activity bins", book.Records())
	}
	stats := ActivityFor(book.Snapshot(), key)
	if stats.Swaps < 7200 || stats.Swaps > 7258 {
		t.Fatal("window retained cumulative history", stats)
	}
	book.Observe(domain.BlockRef{Number: 10001, Hash: domain.Hash{2}}, logs, []domain.PoolKey{key}, 7200)
	if len(book.Unknown()) != 0 || book.Snapshot()[key].Swaps <= 7000 {
		t.Fatal("verified pool lost activity or remained unknown", book.Snapshot())
	}
	restored := ActivityBook{}
	if err := restored.Restore(book.Records(), book.observed); err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot()[key] != book.Snapshot()[key] {
		t.Fatal("restart changed binned statistics")
	}
}

// Pancake V3 pools emit their own Swap signature; their activity counts too.
func TestActivityCountsPancakeV3Swaps(t *testing.T) {
	book := ActivityBook{}
	key := domain.PoolKey{Address: domain.Address{2}, Kind: domain.PoolKindV3}
	ref := domain.BlockRef{Number: 10, Hash: domain.Hash{10}}
	book.Observe(ref, []rpc.Log{{Address: key.Address, Topics: []domain.Hash{uniswapv3.TopicPancakeSwap}}}, []domain.PoolKey{key}, 2)
	if book.Snapshot()[key].Swaps != 1 {
		t.Fatal(book.Snapshot())
	}
}
