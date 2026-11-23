package state

import (
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// A MetricOmm pool's bins and quote are minutes old by the time a snapshot
// is read again, so only its registration is saved: the restored pool keeps
// what is needed to read it and comes back stale, without words or a quote.
func TestSnapshotKeepsMetricRegistrationAndRestoresStale(t *testing.T) {
	key := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: domain.Address{0x2b}}
	metric := &pool.MetricState{
		Token0Scale: big.NewInt(1), Token1Scale: big.NewInt(1_000_000_000_000), LowestBin: -20, HighestBin: 19,
		Words:  make([]domain.Hash, 42),
		Oracle: domain.Address{0xe8}, FeedIndex: 1, OracleSlot: domain.Hash{0x8d},
		QuoteExtension: domain.Address{0xf1}, QuoteExtensionSlot: domain.Hash{0xd2},
		Reference: domain.Address{0x30}, ReferenceBaseFeed: 2, ReferenceQuoteFeed: 7, ReferenceBase1e8: 5, ReferenceQuote1e8: 6,
		BidX64: big.NewInt(100), AskX64: big.NewInt(101), QuoteMS: 1_794_323_390_916,
		SkewConfig: domain.Hash{1}, SkewParameters: domain.Hash{2}, SkewState: domain.Hash{3},
	}
	store := New()
	if err := store.Upsert(pool.State{Key: key, Token0: domain.Address{1}, Token1: domain.Address{2}, Metric: metric, UpdatedBlock: 7}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginBlock(domain.BlockRef{Number: 7, Hash: domain.Hash{7}, ParentHash: domain.Hash{6}}); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version":5`) {
		t.Fatalf("a MetricOmm pool needs snapshot version %d: %s", MetricStateVersion, data)
	}
	restored := New()
	if err := restored.LoadSnapshot(data); err != nil {
		t.Fatal(err)
	}
	p, ok := restored.Pool(key)
	if !ok || p.Metric == nil {
		t.Fatal("MetricOmm pool lost")
	}
	m := p.Metric
	if !m.Stale || m.Words != nil || m.BidX64 != nil || m.AskX64 != nil || m.QuoteMS != 0 || m.SkewState != (domain.Hash{}) {
		t.Fatalf("restored pool carries old state: %+v", m)
	}
	if m.Token0Scale.Cmp(metric.Token0Scale) != 0 || m.Token1Scale.Cmp(metric.Token1Scale) != 0 || m.LowestBin != -20 || m.HighestBin != 19 ||
		m.Oracle != metric.Oracle || m.FeedIndex != 1 || m.OracleSlot != metric.OracleSlot || m.QuoteExtension != metric.QuoteExtension || m.QuoteExtensionSlot != metric.QuoteExtensionSlot ||
		m.Reference != metric.Reference || m.ReferenceBaseFeed != 2 || m.ReferenceQuoteFeed != 7 || m.ReferenceBase1e8 != 0 {
		t.Fatalf("registration changed: %+v", m)
	}
	// Encoding is deterministic, and a restored store encodes to itself.
	again, err := restored.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	second := New()
	if err := second.LoadSnapshot(again); err != nil {
		t.Fatal(err)
	}
	if third, _ := second.EncodeSnapshot(); string(third) != string(again) {
		t.Fatal("snapshot of a restored store is not stable")
	}
	// The registration and the kind go together, and need version 5.
	for _, broken := range []string{
		strings.Replace(string(data), `"version":5`, `"version":4`, 1),
		strings.Replace(string(data), `"kind":8`, `"kind":2`, 1),
	} {
		if err := New().LoadSnapshot([]byte(broken)); err == nil {
			t.Fatal("inconsistent MetricOmm snapshot accepted")
		}
	}
}
