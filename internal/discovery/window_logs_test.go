package discovery

import (
	"context"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type recordedLogSource struct{ ranges []rpc.LogFilter }

func (s *recordedLogSource) GetLogs(_ context.Context, f rpc.LogFilter) ([]rpc.Log, error) {
	s.ranges = append(s.ranges, f)
	logs := []rpc.Log{{BlockNumber: f.FromBlock, Data: []byte{1}}}
	if f.ToBlock != f.FromBlock {
		logs = append(logs, rpc.Log{BlockNumber: f.ToBlock, Data: []byte{2}})
	}
	return logs, nil
}
func TestWindowLogsReuseChunkedHistoryAndReset(t *testing.T) {
	source := &recordedLogSource{}
	cached := &WindowLogs{Source: source, Window: 50}
	filter := rpc.LogFilter{FromBlock: 0, ToBlock: 9, Addresses: []domain.Address{{1}}}
	for i := uint64(0); i < 5; i++ {
		filter.FromBlock = i * 10
		filter.ToBlock = i*10 + 9
		if _, err := cached.GetLogs(t.Context(), filter); err != nil {
			t.Fatal(err)
		}
	}
	// A shifted scan first reuses all covered chunks. Only its final suffix reads RPC.
	for i := uint64(0); i < 5; i++ {
		filter.FromBlock = 3 + i*10
		filter.ToBlock = 12 + i*10
		if _, err := cached.GetLogs(t.Context(), filter); err != nil {
			t.Fatal(err)
		}
	}
	if len(source.ranges) != 6 || source.ranges[5].FromBlock != 50 || source.ranges[5].ToBlock != 52 {
		t.Fatal("rescanned covered history", source.ranges)
	}
	filter.FromBlock, filter.ToBlock = 40, 52
	logs, err := cached.GetLogs(t.Context(), filter)
	if err != nil || len(logs) == 0 {
		t.Fatal(logs, err)
	}
	logs[0].Data[0] = 99
	again, err := cached.GetLogs(t.Context(), filter)
	if err != nil || again[0].Data[0] == 99 {
		t.Fatal("caller mutated the cached log window", err)
	}
	cached.Reset()
	if _, err := cached.GetLogs(t.Context(), filter); err != nil {
		t.Fatal(err)
	}
	if len(source.ranges) != 7 {
		t.Fatal("reorg reset retained stale height cache")
	}
}

type blockedLogSource struct {
	started, release chan struct{}
	calls            int
}

func (s *blockedLogSource) GetLogs(ctx context.Context, filter rpc.LogFilter) ([]rpc.Log, error) {
	s.calls++
	if s.calls == 1 {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []rpc.Log{{BlockNumber: filter.FromBlock}}, nil
}
func TestWindowResetRejectsInFlightOldGenerationCache(t *testing.T) {
	source := &blockedLogSource{started: make(chan struct{}), release: make(chan struct{})}
	cached := &WindowLogs{Source: source, Window: 50}
	filter := rpc.LogFilter{FromBlock: 10, ToBlock: 20}
	done := make(chan error, 1)
	go func() { _, err := cached.GetLogs(t.Context(), filter); done <- err }()
	<-source.started
	cached.Reset()
	close(source.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := cached.GetLogs(t.Context(), filter); err != nil {
		t.Fatal(err)
	}
	if source.calls != 2 {
		t.Fatal("old generation repopulated cache", source.calls)
	}
}
