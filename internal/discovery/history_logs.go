package discovery

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/Rorical/Rosetta/internal/rpc"
)

// ErrHistoryLogsUnavailable marks a failed request to the dedicated history
// logs endpoint. The V4 backfill degrades for the round instead of failing it.
var ErrHistoryLogsUnavailable = errors.New("history logs unavailable")

// HistoryLogs serves exact-ID V4 Initialize lookups from a provider without
// the primary RPC's eth_getLogs block-range cap. A pool initialized months ago
// is found by its pool ID topic in one request per manager.
type HistoryLogs struct {
	Source LogSource
}

func (h HistoryLogs) GetLogs(ctx context.Context, filter rpc.LogFilter) ([]rpc.Log, error) {
	logs, err := h.Source.GetLogs(ctx, filter)
	if err == nil || ctx.Err() != nil {
		return logs, err
	}
	// The configured endpoint can carry an API key; keep only the cause.
	var request *url.Error
	if errors.As(err, &request) {
		err = request.Err
	}
	return nil, fmt.Errorf("%w: %v", ErrHistoryLogsUnavailable, err)
}
