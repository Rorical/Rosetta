package discovery

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestCandidateQueueRotatesPastRepeatedlyRejectedLeaders(t *testing.T) {
	rows := make([]Creation, 6)
	activity := map[domain.PoolKey]Activity{}
	for i := range rows {
		rows[i].Spec = Spec{Address: domain.Address{byte(i + 1)}, Kind: domain.PoolKindV2}
		activity[rows[i].Spec.PoolKey()] = Activity{Swaps: uint64(100 - i)}
	}
	queue := CandidateQueue{}
	seen := map[domain.PoolKey]bool{}
	for round := 0; round < 3; round++ {
		batch := queue.Next(rows, activity, 2)
		for _, row := range batch {
			if seen[row.Spec.PoolKey()] {
				t.Fatal("high-activity rejections starved unchecked markets")
			}
			seen[row.Spec.PoolKey()] = true
		}
	}
	if len(seen) != 6 {
		t.Fatal("not all indexed markets were checked")
	}
	if got := queue.Next(rows, activity, 2); got[0].Spec.Address != rows[0].Spec.Address {
		t.Fatal("old markets never became eligible for a state refresh")
	}
}

func TestProbeAllowanceReservesUnknownMarketWork(t *testing.T) {
	for _, test := range []struct{ limit, indexed, want int }{{32, 100, 8}, {32, 2, 30}, {1, 100, 1}} {
		if got := ProbeAllowance(test.limit, test.indexed); got != test.want {
			t.Fatalf("limit=%d indexed=%d: got %d want %d", test.limit, test.indexed, got, test.want)
		}
	}
}
