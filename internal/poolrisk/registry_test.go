package poolrisk

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

var opChain = domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}

func pool(n byte) domain.PoolKey {
	return domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{n}}
}

func TestTrackerQuarantinesOnlyPoolsThatFailAcrossRoutesWithoutSuccess(t *testing.T) {
	broken, healthy, other := pool(1), pool(2), pool(3)
	block := domain.BlockRef{Number: 7, Hash: domain.Hash{7}}
	var tracker Tracker
	// The healthy pool shares the first failing route but succeeds elsewhere.
	if got := tracker.Observe(domain.Hash{1}, []domain.PoolKey{broken, healthy}, true, block); len(got) != 0 {
		t.Fatal("quarantined after one revert", got)
	}
	tracker.Observe(domain.Hash{9}, []domain.PoolKey{healthy, other}, false, block)
	if got := tracker.Observe(domain.Hash{1}, []domain.PoolKey{broken, healthy}, true, block); len(got) != 0 {
		t.Fatal("one route is not enough", got)
	}
	got := tracker.Observe(domain.Hash{2}, []domain.PoolKey{broken, other, broken}, true, block)
	if len(got) != 1 || got[0].Pool != broken || got[0].Reverts != 3 || got[0].Routes != 2 {
		t.Fatal("broken pool not quarantined alone", got)
	}
	// A pool that has succeeded is never blamed, even when decoys revert with it.
	for i := byte(10); i < 15; i++ {
		decoy := pool(i)
		for _, e := range tracker.Observe(domain.Hash{i}, []domain.PoolKey{decoy, healthy}, true, block) {
			if e.Pool == healthy {
				t.Fatal("proven pool quarantined through decoy routes", e)
			}
		}
	}
}

func TestRegistryPersistsQuarantinePerChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool-risks.jsonl")
	registry, err := Open(path, opChain)
	if err != nil {
		t.Fatal(err)
	}
	evidence := Evidence{Pool: pool(1), Reverts: 3, Routes: 2, Block: domain.BlockRef{Number: 7, Hash: domain.Hash{7}}, Source: "execution_paper"}
	if err := registry.Record(evidence); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(registry.Check(pool(1)), ErrQuarantined) || registry.Check(pool(2)) != nil {
		t.Fatal("quarantine not applied to exactly the recorded pool")
	}
	if err := registry.Record(Evidence{Pool: pool(2)}); err == nil {
		t.Fatal("incomplete evidence accepted")
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, opChain)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(reopened.Check(pool(1)), ErrQuarantined) {
		t.Fatal("quarantine lost across restart")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}); err == nil {
		t.Fatal("journal loaded for another chain")
	}
}
