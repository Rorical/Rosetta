package arb

import (
	"context"
	"errors"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type memoQuoter struct {
	calls atomic.Int64
	fail  atomic.Bool
}

func (q *memoQuoter) QuoteExactIn(_ context.Context, p pool.State, _ pool.DeepState, token domain.Address, n *big.Int) (*big.Int, error) {
	q.calls.Add(1)
	if q.fail.Load() {
		return nil, ErrMissingDeep
	}
	out := new(big.Int).Add(n, p.Reserve0)
	if token == p.Token1 {
		out.Neg(out)
	}
	return out, nil
}

func TestExactLegMemoOwnsResultsAndSeparatesDirectionAmountAndJob(t *testing.T) {
	q := new(memoQuoter)
	memo := newLegQuoteMemo()
	p := testV2State(1, 1, 2, 100, 200)
	first, err := memo.quote(t.Context(), q, p, pool.DeepState{}, p.Token0, big.NewInt(7))
	if err != nil {
		t.Fatal(err)
	}
	first.SetInt64(999)
	cached, _ := memo.quote(t.Context(), q, p, pool.DeepState{}, p.Token0, big.NewInt(7))
	if cached.Int64() != 107 || q.calls.Load() != 1 {
		t.Fatal("cache aliases caller or repeats same quote")
	}
	reverse, _ := memo.quote(t.Context(), q, p, pool.DeepState{}, p.Token1, big.NewInt(7))
	other, _ := memo.quote(t.Context(), q, p, pool.DeepState{}, p.Token0, big.NewInt(8))
	if reverse.Int64() != -107 || other.Int64() != 108 || q.calls.Load() != 3 {
		t.Fatal("memo conflated exact inputs")
	}
	p.Reserve0 = big.NewInt(300)
	fresh, _ := newLegQuoteMemo().quote(t.Context(), q, p, pool.DeepState{}, p.Token0, big.NewInt(7))
	if fresh.Int64() != 307 {
		t.Fatal("new job reused old state")
	}
}

func TestExactLegMemoRetriesErrorsAndBoundsStorage(t *testing.T) {
	q := new(memoQuoter)
	memo := newLegQuoteMemo()
	p := testV2State(1, 1, 2, 100, 200)
	q.fail.Store(true)
	for range 2 {
		if _, err := memo.quote(t.Context(), q, p, pool.DeepState{}, p.Token0, big.NewInt(7)); !errors.Is(err, ErrMissingDeep) {
			t.Fatal(err)
		}
	}
	q.fail.Store(false)
	for i := int64(1); i <= 6000; i++ {
		if _, err := memo.quote(t.Context(), q, p, pool.DeepState{}, p.Token0, big.NewInt(i)); err != nil {
			t.Fatal(err)
		}
	}
	if q.calls.Load() != 6002 {
		t.Fatal("failed quote was cached")
	}
	total := 0
	for i := range memo.shards {
		n := len(memo.shards[i].values)
		if n > 256 {
			t.Fatal("unbounded shard", n)
		}
		total += n
	}
	if total > 4096 {
		t.Fatal("unbounded memo", total)
	}
}
