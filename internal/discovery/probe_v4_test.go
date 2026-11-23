package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestV4BackfillFindsOldInitializeAndCachesAcrossRestart(t *testing.T) {
	log := v4Log(t, repeatedAddress(1), repeatedAddress(2), 500, 10, domain.Address{})
	log.BlockNumber = 3
	source := newFakeLogs()
	source.add(log)
	index := &CreationIndex{Source: source, Path: filepath.Join(t.TempDir(), "index.json"), ChainID: 10}
	key := domain.PoolKey{Address: log.Address, Kind: domain.PoolKindV4, PoolID: log.Topics[1]}
	options := ProbeOptions{Emitters: []Emitter{{SourceUniswapV4, log.Address}}, Seen: map[domain.PoolKey]bool{}, Activity: map[domain.PoolKey]Activity{key: {Swaps: 2, MultiSwapUses: 1}}, Anchor: domain.BlockRef{Number: 100, Hash: domain.Hash{1}}, Limit: 4, Cooldown: 20}
	probe := ActiveProbe{}
	rows, err := probe.RunWithLogs(t.Context(), nil, index, options)
	if err != nil || len(rows) != 1 || rows[0].Block != 3 {
		t.Fatal(rows, err)
	}
	if len(source.filters) != 1 || source.filters[0].FromBlock != 0 || source.filters[0].ToBlock != 100 || len(source.filters[0].Topics[1]) != 1 || source.filters[0].Topics[1][0] != key.PoolID {
		t.Fatal("historical probe was not exact-ID bounded", source.filters)
	}
	index.Remember(rows)
	if err := index.Save(t.Context(), options.Anchor); err != nil {
		t.Fatal(err)
	}
	restored := &CreationIndex{Source: source, Path: index.Path, ChainID: 10}
	if _, err := restored.Load(); err != nil {
		t.Fatal(err)
	}
	options.Metadata = restored.Records()
	options.Seen = map[domain.PoolKey]bool{}
	probe = ActiveProbe{}
	rows, err = probe.RunWithLogs(t.Context(), nil, restored, options)
	if err != nil || len(rows) != 0 || len(source.filters) != 1 {
		t.Fatal("re-fetched immutable Initialize", rows, err)
	}
}
func TestV4BackfillKeepsIDsSeparateAndRejectsForgedMetadata(t *testing.T) {
	a := v4Log(t, repeatedAddress(1), repeatedAddress(2), 500, 10, domain.Address{})
	b := v4Log(t, repeatedAddress(1), repeatedAddress(2), 3000, 60, repeatedAddress(4))
	source := newFakeLogs()
	source.add(a, b)
	ka := domain.PoolKey{Address: a.Address, Kind: domain.PoolKindV4, PoolID: a.Topics[1]}
	kb := ka
	kb.PoolID = b.Topics[1]
	options := ProbeOptions{Emitters: []Emitter{{SourceUniswapV4, a.Address}}, Activity: map[domain.PoolKey]Activity{ka: {Swaps: 1}, kb: {Swaps: 1}}, Seen: map[domain.PoolKey]bool{}, Limit: 4, Anchor: domain.BlockRef{Number: 100}, Cooldown: 10}
	p := ActiveProbe{}
	rows, err := p.RunWithLogs(t.Context(), nil, source, options)
	if err != nil || len(rows) != 2 || len(source.filters) != 1 {
		t.Fatal(rows, err)
	}
	hooks := false
	for _, r := range p.Report {
		hooks = hooks || r.Reason == "unsupported_v4_hooks"
	}
	if !hooks {
		t.Fatal("unsupported hook metadata was hidden")
	}
	p = ActiveProbe{}
	options.Seen = map[domain.PoolKey]bool{}
	source = newFakeLogs()
	a.Data[31] ^= 1
	source.add(a)
	rows, err = p.RunWithLogs(t.Context(), nil, source, options)
	if err != nil || len(rows) != 0 {
		t.Fatal("forged key admitted", rows, err)
	}
	source.err = rpc.ErrCallBudgetExceeded
	p = ActiveProbe{}
	if _, err = p.RunWithLogs(t.Context(), nil, source, options); !errors.Is(err, rpc.ErrCallBudgetExceeded) {
		t.Fatal("budget failure was hidden", err)
	}
}
func TestMultiSwapEvidencePrioritizesMissingMarketsWithoutClaimingProfit(t *testing.T) {
	a, b, c := repeatedAddress(1), repeatedAddress(2), repeatedAddress(3)
	logs := []rpc.Log{{Address: a, TxHash: domain.Hash{1}, Topics: []domain.Hash{swapV3}}, {Address: b, TxHash: domain.Hash{1}, Topics: []domain.Hash{swapV3}}, {Address: b, TxHash: domain.Hash{1}, Topics: []domain.Hash{swapV3}}, {Address: c, TxHash: domain.Hash{2}, Topics: []domain.Hash{swapV3}}}
	ref := domain.BlockRef{Number: 100, Hash: domain.Hash{1}}
	book := ActivityBook{}
	book.Observe(ref, logs, nil, 10)
	book.Observe(ref, logs, nil, 10)
	stats := book.Snapshot()
	ka := domain.PoolKey{Address: a}
	kb := domain.PoolKey{Address: b}
	kc := domain.PoolKey{Address: c}
	if stats[ka].MultiSwapUses != 1 || stats[kb].MultiSwapUses != 1 || stats[kc].MultiSwapUses != 0 || stats[ka].ProfitableUses != 0 {
		t.Fatal("route evidence mislabeled", stats)
	}
	restored := ActivityBook{}
	if err := restored.Restore(book.Records(), ref); err != nil {
		t.Fatal(err)
	}
	verified := ka
	verified.Kind = domain.PoolKindV3
	restored.Observe(domain.BlockRef{Number: 101, Hash: domain.Hash{2}}, nil, []domain.PoolKey{verified}, 10)
	if restored.Snapshot()[verified].MultiSwapUses != 1 {
		t.Fatal("classification lost route priority")
	}
	// A busier single-swap market must not displace an observed route's unknown pool.
	stats[kc] = Activity{Swaps: 100}
	q := CandidateQueue{}
	rows := q.Next([]Creation{{Spec: Spec{Address: c}}, {Spec: Spec{Address: a}}}, stats, 1)
	if len(rows) != 1 || rows[0].Spec.Address != a {
		t.Fatal("routed market lost priority", rows)
	}
}

// A Nitro follower can time out on a V4 Initialize filter wider than a few
// million blocks (a million takes about 2 s), so a full-history backfill of a
// fast chain never completes a discovery round. With a history
// chunk the backfill walks back from the anchor and stops once every pool's
// Initialize is found.
func TestV4BackfillWalksBackInChunks(t *testing.T) {
	old := v4Log(t, repeatedAddress(1), repeatedAddress(2), 500, 10, domain.Address{})
	old.BlockNumber = 3
	recent := v4Log(t, repeatedAddress(1), repeatedAddress(2), 3000, 60, domain.Address{})
	recent.BlockNumber = 95
	source := newFakeLogs()
	source.maxRange = 30
	source.add(old, recent)
	keyOf := func(log rpc.Log) domain.PoolKey {
		return domain.PoolKey{Address: log.Address, Kind: domain.PoolKindV4, PoolID: log.Topics[1]}
	}
	options := ProbeOptions{Emitters: []Emitter{{SourceUniswapV4, old.Address}}, Seen: map[domain.PoolKey]bool{}, Activity: map[domain.PoolKey]Activity{keyOf(old): {Swaps: 2}, keyOf(recent): {Swaps: 2}}, Anchor: domain.BlockRef{Number: 100}, Limit: 4, Cooldown: 20, HistoryChunk: 30}
	probe := ActiveProbe{}
	rows, err := probe.RunWithLogs(t.Context(), nil, source, options)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	ranges := [][2]uint64{}
	for _, f := range source.filters {
		ranges = append(ranges, [2]uint64{f.FromBlock, f.ToBlock})
	}
	if want := [][2]uint64{{71, 100}, {41, 70}, {11, 40}, {0, 10}}; !slices.Equal(ranges, want) {
		t.Fatalf("ranges %v, want %v", ranges, want)
	}
	if len(source.filters[1].Topics[1]) != 1 || source.filters[1].Topics[1][0] != keyOf(old).PoolID {
		t.Fatalf("a found pool was asked for again: %v", source.filters[1].Topics[1])
	}

	// Found within the first chunk: one query.
	source.filters = nil
	options.Activity = map[domain.PoolKey]Activity{keyOf(recent): {Swaps: 2}}
	options.Seen = map[domain.PoolKey]bool{}
	probe = ActiveProbe{}
	if rows, err = probe.RunWithLogs(t.Context(), nil, source, options); err != nil || len(rows) != 1 || len(source.filters) != 1 {
		t.Fatalf("rows %v err %v after %d queries", rows, err, len(source.filters))
	}
}

// A round's state reads must finish before the follower prunes the anchor's
// state (8,192 blocks, about 14 minutes at 100 ms blocks); a full chunked
// backfill can take longer, so every round would fail and none complete.
// With a per-round chunk budget the scan resumes next round
// where it stopped, and pools found so far are admitted at once.
func TestV4BackfillResumesAcrossRounds(t *testing.T) {
	old := v4Log(t, repeatedAddress(1), repeatedAddress(2), 500, 10, domain.Address{})
	old.BlockNumber = 3
	recent := v4Log(t, repeatedAddress(1), repeatedAddress(2), 3000, 60, domain.Address{})
	recent.BlockNumber = 95
	source := newFakeLogs()
	source.add(old, recent)
	keyOf := func(log rpc.Log) domain.PoolKey {
		return domain.PoolKey{Address: log.Address, Kind: domain.PoolKindV4, PoolID: log.Topics[1]}
	}
	activity := map[domain.PoolKey]Activity{keyOf(old): {Swaps: 2}, keyOf(recent): {Swaps: 2}}
	options := ProbeOptions{Emitters: []Emitter{{SourceUniswapV4, old.Address}}, Seen: map[domain.PoolKey]bool{}, Activity: activity, Anchor: domain.BlockRef{Number: 100}, Limit: 4, Cooldown: 20, HistoryChunk: 30, HistoryChunksPerRound: 2}
	probe := ActiveProbe{}
	rows, err := probe.RunWithLogs(t.Context(), nil, source, options)
	if err != nil || len(rows) != 1 || rows[0].Block != 95 {
		t.Fatalf("first round: %v %v", rows, err)
	}
	if _, cooling := probe.After[keyOf(old)]; cooling {
		t.Fatal("a pool still being scanned was put on cooldown")
	}
	scanning := false
	for _, r := range probe.Report {
		scanning = scanning || r.Pool == keyOf(old) && r.Reason == "v4_initialize_scanning"
	}
	if !scanning {
		t.Fatalf("pending pool not reported as scanning: %+v", probe.Report)
	}
	source.filters = nil
	options.Anchor = domain.BlockRef{Number: 110}
	options.Seen = map[domain.PoolKey]bool{keyOf(recent): true}
	rows, err = probe.RunWithLogs(t.Context(), nil, source, options)
	if err != nil || len(rows) != 1 || rows[0].Block != 3 {
		t.Fatalf("second round: %v %v", rows, err)
	}
	ranges := [][2]uint64{}
	for _, f := range source.filters {
		ranges = append(ranges, [2]uint64{f.FromBlock, f.ToBlock})
	}
	if want := [][2]uint64{{11, 40}, {0, 10}}; !slices.Equal(ranges, want) {
		t.Fatalf("second round ranges %v, want %v", ranges, want)
	}
}

// A range that fails mid-round must not drop pools found earlier that round.
func TestV4BackfillFailureKeepsFoundPools(t *testing.T) {
	recent := v4Log(t, repeatedAddress(1), repeatedAddress(2), 3000, 60, domain.Address{})
	recent.BlockNumber = 95
	old := v4Log(t, repeatedAddress(1), repeatedAddress(2), 500, 10, domain.Address{})
	old.BlockNumber = 3
	source := newFakeLogs()
	source.add(recent, old)
	keyOf := func(log rpc.Log) domain.PoolKey {
		return domain.PoolKey{Address: log.Address, Kind: domain.PoolKindV4, PoolID: log.Topics[1]}
	}
	options := ProbeOptions{Emitters: []Emitter{{SourceUniswapV4, recent.Address}}, Seen: map[domain.PoolKey]bool{}, Activity: map[domain.PoolKey]Activity{keyOf(old): {Swaps: 2}, keyOf(recent): {Swaps: 2}}, Anchor: domain.BlockRef{Number: 100}, Limit: 4, Cooldown: 20, HistoryChunk: 30}
	failing := &failAfter{fakeLogs: source, ok: 1}
	probe := ActiveProbe{}
	if _, err := probe.RunWithLogs(t.Context(), nil, failing, options); err == nil {
		t.Fatal("a failed range was hidden")
	}
	rows, err := probe.RunWithLogs(t.Context(), nil, source, options)
	if err != nil || len(rows) != 2 {
		t.Fatalf("after the failure: %v %v", rows, err)
	}
}

// failAfter answers ok queries, then fails like a timed-out node.
type failAfter struct {
	*fakeLogs
	ok int
}

func (f *failAfter) GetLogs(ctx context.Context, filter rpc.LogFilter) ([]rpc.Log, error) {
	if f.ok == 0 {
		return nil, &rpc.RPCError{Code: -32002, Message: "request timed out"}
	}
	f.ok--
	return f.fakeLogs.GetLogs(ctx, filter)
}
