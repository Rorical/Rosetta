package pool

import (
	"encoding/json"
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestMetricStateCopyIsDeep(t *testing.T) {
	original := State{
		Key: domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindMetricOmm},
		Metric: &MetricState{
			Token0Scale: big.NewInt(1),
			Token1Scale: big.NewInt(1_000_000_000_000),
			LowestBin:   -1,
			HighestBin:  0,
			Words:       []domain.Hash{{1}, {2}, {3}, {4}},
			Oracle:      domain.Address{9},
			FeedIndex:   1,
			BidX64:      big.NewInt(100),
			AskX64:      big.NewInt(101),
			QuoteMS:     1794423732633,
		},
	}
	copied := original.Copy()
	if !reflect.DeepEqual(copied, original) {
		t.Fatalf("copy differs: %+v", copied.Metric)
	}

	copied.Metric.Words[0] = domain.Hash{7}
	copied.Metric.Token1Scale.SetInt64(5)
	copied.Metric.BidX64.SetInt64(5)
	copied.Metric.AskX64 = nil
	copied.Metric.Stale = true
	if original.Metric.Words[0] != (domain.Hash{1}) || original.Metric.Token1Scale.Int64() != 1_000_000_000_000 ||
		original.Metric.BidX64.Int64() != 100 || original.Metric.AskX64.Int64() != 101 || original.Metric.Stale {
		t.Fatalf("original mutated through the copy: %+v", original.Metric)
	}

	if (State{}).Copy().Metric != nil {
		t.Fatal("copy of a state without MetricOmm data grew one")
	}
}

// The state is persisted in snapshots: an unknown quote must stay unknown.
func TestMetricStateJSONRoundTrip(t *testing.T) {
	for _, original := range []MetricState{
		{Token0Scale: big.NewInt(1), Token1Scale: big.NewInt(10), LowestBin: -2, HighestBin: 1,
			Words: []domain.Hash{{0xff}, {31: 1}}, Oracle: domain.Address{9}, FeedIndex: 2,
			BidX64: new(big.Int).Lsh(big.NewInt(3), 70), AskX64: new(big.Int).Lsh(big.NewInt(5), 70), QuoteMS: 42},
		{Token0Scale: big.NewInt(1), Token1Scale: big.NewInt(1), Stale: true},
	} {
		raw, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		var decoded MetricState
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, original) {
			t.Fatalf("round trip changed the state: %s", raw)
		}
	}
}
