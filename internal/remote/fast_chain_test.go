package remote

import (
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestArbitrumRPCFastPoolsLeadCanonicalByMultipleBlocks(t *testing.T) {
	r, pools := flashChainRuntime(t)
	r.cfg = config.Config{FastURL: "enabled"}
	arrived := time.Now().Add(-time.Second)
	var first feed.FastMessage
	for number := uint64(11); number <= 14; number++ {
		message := feed.FastMessage{BlockNumber: number, TxHash: domain.Hash{byte(number)}, RawTx: []byte{byte(number)}, Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, int64(number)*300000)}}}
		if number == 11 {
			first = message
		}
		if err := r.acceptFastMessage(t.Context(), message, arrived, 0); err != nil {
			t.Fatal(err)
		}
	}
	_, old, _, ok := r.fast.View()
	if !ok || old.BlockNumber != 14 || old.Anchor.Number != 10 {
		t.Fatal("Fast Feed waited for canonical parent")
	}
	parent := r.store.Block()
	parent.Number = 11
	parent.ParentHash = parent.Hash
	parent.Hash = domain.Hash{11}
	log := first.Receipt.Logs[0]
	tx := ingest.Transaction{Hash: first.TxHash, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}}
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent, Transactions: []domain.Hash{tx.Hash}}, transactions: []ingest.Transaction{tx}}); err != nil {
		t.Fatal(err)
	}
	view, version, _, ok := r.fast.View()
	if !ok || version.BlockNumber != 14 || version.Anchor != parent || r.Current(old) {
		t.Fatal("background RPC dropped speculative multi-block suffix")
	}
	p, _ := view.Pool(pools[0].Key)
	if p.Reserve1.Int64() != 4_200_000 {
		t.Fatal("future pool state lost during canonical rebase")
	}
	if r.fastReceivedAt != arrived {
		t.Fatal("canonical rebase reset the Fast Feed arrival time")
	}
	// Refreshing another dependency must also resubmit the retained Fast paths;
	// their previous worker ticket is retired by the refresh.
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSearch{}
	r.topology, r.worker = topology, &arb.Worker{}
	r.search = pipeline.NewSearchScheduler(sink, 1, nil)
	r.cfg.RPCSearchRefresh = "logs"
	if err := r.refreshTentativeDependencies(t.Context(), []domain.PoolKey{pools[1].Key}); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 1 || len(sink.jobs[0].Dirty) != 2 || !slices.Contains(sink.jobs[0].Dirty, pools[0].Key) || !slices.Contains(sink.jobs[0].Dirty, pools[1].Key) {
		t.Fatal("dependency refresh lost retained Fast paths")
	}
}

// A live Fast Feed leads the canonical view, so standard blocks are neither
// searched nor admitted; once it goes quiet they are again.
func TestStreamActiveFollowsTheFastFeed(t *testing.T) {
	now := time.Now()
	r := &runtime{cfg: config.Config{Network: config.Network{Family: domain.FamilyNitro, ChainID: 42161}}}
	if r.streamActive(now) {
		t.Fatal("active without a stream")
	}
	r.fastReceivedAt = now.Add(-time.Second)
	if r.streamActive(now) {
		t.Fatal("active without a Fast Feed configured")
	}
	r.cfg.FastURL = "wss://fast.invalid"
	if !r.streamActive(now) {
		t.Fatal("inactive one second after a Fast message")
	}
	if r.streamActive(now.Add(fastQuiet)) {
		t.Fatal("still active after the feed went quiet")
	}
}

// A clean canonical reconcile searches the block's pools and the pools its
// replayed Fast suffix changed, not the whole table: every pool at every block
// would rank Arbitrum One's complete table four times a second.
func TestFastReconcileSearchesTheBlockAndSuffixPools(t *testing.T) {
	r, pools := flashChainRuntime(t)
	r.cfg = config.Config{FastURL: "enabled"}
	if len(pools) < 2 {
		t.Fatal("fixture needs an untouched pool")
	}
	var first feed.FastMessage
	for number := uint64(11); number <= 12; number++ {
		message := feed.FastMessage{BlockNumber: number, TxHash: domain.Hash{byte(number)}, RawTx: []byte{byte(number)}, Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, int64(number)*300000)}}}
		if number == 11 {
			first = message
		}
		if err := r.acceptFastMessage(t.Context(), message, time.Now().Add(-time.Second), 0); err != nil {
			t.Fatal(err)
		}
	}
	topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSearch{}
	r.topology, r.worker = topology, &arb.Worker{}
	r.search = pipeline.NewSearchScheduler(sink, 1, nil)
	parent := r.store.Block()
	parent.Number = 11
	parent.ParentHash = parent.Hash
	parent.Hash = domain.Hash{11}
	log := first.Receipt.Logs[0]
	tx := ingest.Transaction{Hash: first.TxHash, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}}
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent, Transactions: []domain.Hash{tx.Hash}}, transactions: []ingest.Transaction{tx}}); err != nil {
		t.Fatal(err)
	}
	if len(sink.jobs) != 1 || !slices.Equal(sink.jobs[0].Dirty, []domain.PoolKey{pools[0].Key}) {
		t.Fatal("reconcile searched other pools than the block and its suffix", len(sink.jobs))
	}
	// The canonical block triggers this search, not the last Fast frame: that
	// frame's arrival would age reconcile offers by up to a round.
	if age := time.Since(sink.jobs[0].ReceivedAt); age > 500*time.Millisecond {
		t.Fatal("reconcile search dated by the last Fast frame", age)
	}
}
