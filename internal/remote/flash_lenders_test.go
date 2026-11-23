package remote

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// The lender list keeps only V3 pools and is rebuilt when the pool set grows,
// so a newly discovered pool can fund a loan.
func TestFlashLendersFollowThePoolSet(t *testing.T) {
	r, _ := reactiveFixture(t)
	if got := r.flashLenders(); len(got) != 0 {
		t.Fatal("V2 pools listed as flash lenders", len(got))
	}
	v3 := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: addressAt(30)}, Token0: addressAt(1), Token1: addressAt(2), Fee: 500, TickSpacing: 10,
		SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1_000_000)}
	if err := r.store.Upsert(v3); err != nil {
		t.Fatal(err)
	}
	got := r.flashLenders()
	if len(got) != 1 || got[0].Key != v3.Key {
		t.Fatal("new V3 pool missing from lenders", got)
	}
}
