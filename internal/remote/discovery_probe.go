package remote

import (
	"context"

	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
)

func (d *poolDiscovery) probeUnknown(ctx context.Context, job discoveryTask, seen map[domain.PoolKey]bool, limit int, metadata []discovery.Creation) ([]discovery.Creation, error) {
	options := discovery.ProbeOptions{Emitters: d.config.Emitters(), Metadata: metadata, Activity: job.activity, Known: job.view.PoolKeys(), Seen: seen, Anchor: job.reader.block.Ref, Limit: limit, Cooldown: d.config.RefreshBlocks, HistoryChunk: d.config.HistoryChunkBlocks, HistoryChunksPerRound: d.config.HistoryChunksPerRound}
	if d.config.Selection != nil {
		options.MinSwaps = d.config.Selection.MinSwaps
		options.Cooldown = d.config.Selection.ActivityWindow
	}
	logs := d.logs
	if d.history != nil {
		logs = d.history
	}
	return d.probe.RunWithLogs(ctx, job.reader, logs, options)
}
