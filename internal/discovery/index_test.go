package discovery

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestCreationIndexRestartReadsOnlySuffixAndBindsChain(t *testing.T) {
	source := &recordedLogSource{}
	index := &CreationIndex{Source: source, Path: filepath.Join(t.TempDir(), "index.json"), ChainID: 10, Identity: domain.Hash{1}}
	filter := rpc.LogFilter{FromBlock: 10, ToBlock: 20, Addresses: []domain.Address{{1}}}
	if _, err := index.GetLogs(t.Context(), filter); err != nil {
		t.Fatal(err)
	}
	anchor := domain.BlockRef{Number: 20, Hash: domain.Hash{20}}
	if err := index.Save(t.Context(), anchor); err != nil {
		t.Fatal(err)
	}
	restarted := &CreationIndex{Source: source, Path: index.Path, ChainID: 10, Identity: index.Identity}
	if got, err := restarted.Load(); err != nil || got != anchor {
		t.Fatal(got, err)
	}
	filter.ToBlock = 25
	if _, err := restarted.GetLogs(t.Context(), filter); err != nil {
		t.Fatal(err)
	}
	if len(source.ranges) != 2 || source.ranges[1].FromBlock != 21 {
		t.Fatal("repeated historical reads", source.ranges)
	}
	restarted.ChainID = 42161
	if _, err := restarted.Load(); err == nil {
		t.Fatal("index crossed chains")
	}
}
func TestCreationIndexResetDuringRead(t *testing.T) {
	source := &blockedLogSource{started: make(chan struct{}), release: make(chan struct{})}
	index := &CreationIndex{Source: source}
	done := make(chan error, 1)
	go func() {
		_, err := index.GetLogs(context.Background(), rpc.LogFilter{FromBlock: 1, ToBlock: 2})
		done <- err
	}()
	<-source.started
	index.Reset()
	close(source.release)
	if err := <-done; err == nil {
		t.Fatal("orphan scan populated a new index generation")
	}
}

// Adding a discovery source changes the identity. The index is a rebuildable
// cache, so the process must start empty instead of refusing to run.
func TestCreationIndexRebuildsAfterEmitterChange(t *testing.T) {
	source := &recordedLogSource{}
	index := &CreationIndex{Source: source, Path: filepath.Join(t.TempDir(), "index.json"), ChainID: 10, Identity: domain.Hash{1}}
	if _, err := index.GetLogs(t.Context(), rpc.LogFilter{FromBlock: 10, ToBlock: 20, Addresses: []domain.Address{{1}}}); err != nil {
		t.Fatal(err)
	}
	if err := index.Save(t.Context(), domain.BlockRef{Number: 20, Hash: domain.Hash{20}}); err != nil {
		t.Fatal(err)
	}
	changed := &CreationIndex{Source: source, Path: index.Path, ChainID: 10, Identity: domain.Hash{2}}
	if got, err := changed.Load(); err != nil || got != (domain.BlockRef{}) || len(changed.Records()) != 0 {
		t.Fatal("changed emitters should start an empty index", got, err)
	}
}

func TestCreationIndexPersistsV4ScanCursorAndDropsStale(t *testing.T) {
	source := &recordedLogSource{}
	id := domain.Hash{9}
	index := &CreationIndex{Source: source, Path: filepath.Join(t.TempDir(), "index.json"), ChainID: 10, Identity: domain.Hash{1}}
	manager := domain.Address{0x83}
	live := V4ScanState{Manager: manager, End: 500, Pending: []domain.PoolKey{{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{1}}}}
	stale := []V4ScanState{
		{Manager: domain.Address{0x84}, End: 5000, Pending: []domain.PoolKey{{PoolID: domain.Hash{2}}}}, // End above the anchor
		{Manager: domain.Address{0x85}, End: 0, Pending: []domain.PoolKey{{PoolID: domain.Hash{3}}}},    // nothing left below
		{Manager: domain.Address{0x86}, End: 100},                                                       // no pending IDs
	}
	index.SetVerdicts(id, nil, nil, append([]V4ScanState{live}, stale...))
	anchor := domain.BlockRef{Number: 1000, Hash: domain.Hash{20}}
	if err := index.Save(t.Context(), anchor); err != nil {
		t.Fatal(err)
	}
	restarted := &CreationIndex{Source: source, Path: index.Path, ChainID: 10, Identity: index.Identity}
	if _, err := restarted.Load(); err != nil {
		t.Fatal(err)
	}
	got := restarted.V4Scans(id)
	if len(got) != 1 || got[0].Manager != manager || got[0].End != 500 || len(got[0].Pending) != 1 {
		t.Fatal("scan cursor not restored or stale not dropped", got)
	}
	if restarted.V4Scans(domain.Hash{7}) != nil {
		t.Fatal("scans served under another configuration")
	}

	var probe ActiveProbe
	probe.RestoreScans(got)
	if probe.v4Scans[manager] == nil || probe.v4Scans[manager].end != 500 || len(probe.v4Scans[manager].pending) != 1 {
		t.Fatal("probe did not resume the scan", probe.v4Scans)
	}
	if probe.v4Scans[manager].pending[domain.Hash{1}].Address != manager {
		t.Fatal("pending pool key lost its identity")
	}
	round := probe.ScanStates()
	if len(round) != 1 || round[0].End != 500 || len(round[0].Pending) != 1 {
		t.Fatal("scan state did not round-trip", round)
	}
}
