package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// PnLRow totals settled sends of one UTC day and start asset. Asset amounts are
// base units and native amounts wei, summed as floats for display.
type PnLRow struct {
	Day          string  `json:"day"`
	Asset        string  `json:"asset"`
	Settled      int     `json:"settled"`
	Succeeded    int     `json:"succeeded"`
	Reverted     int     `json:"reverted"`
	Cancelled    int     `json:"cancelled"`
	Profit       float64 `json:"profit"`
	Net          float64 `json:"net"`
	Fee          float64 `json:"fee"`
	ProfitNative float64 `json:"profit_native"`
	NetNative    float64 `json:"net_native"`
}

func (s *Store) PnL(ctx context.Context, instance string) ([]PnLRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT date(settled_ms/1000, 'unixepoch') AS day, asset, COUNT(*), SUM(success=1 AND cancelled=0), SUM(success=0 AND cancelled=0), SUM(cancelled),
		COALESCE(SUM(profit_value), 0), COALESCE(SUM(net_value), 0), COALESCE(SUM(fee_value), 0), COALESCE(SUM(profit_native), 0), COALESCE(SUM(net_native), 0)
		FROM sends WHERE instance=? AND success IS NOT NULL GROUP BY day, asset ORDER BY day DESC, asset`, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	days := []PnLRow{}
	for rows.Next() {
		var row PnLRow
		if err := rows.Scan(&row.Day, &row.Asset, &row.Settled, &row.Succeeded, &row.Reverted, &row.Cancelled, &row.Profit, &row.Net, &row.Fee, &row.ProfitNative, &row.NetNative); err != nil {
			return nil, err
		}
		days = append(days, row)
	}
	return days, rows.Err()
}

type SkipCount struct {
	Reason string `json:"reason"`
	Count  uint64 `json:"count"`
}

// PathStats summarizes one submission path's acknowledgement times.
type PathStats struct {
	Name     string `json:"name"`
	Sends    int    `json:"sends"`
	Accepted int    `json:"accepted"`
	P50MS    int64  `json:"p50_ms"`
	P90MS    int64  `json:"p90_ms"`
	MaxMS    int64  `json:"max_ms"`
}

type Survival struct {
	AtMS int64         `json:"at_ms"`
	Rows []SurvivalRow `json:"rows"`
}

// Execution is the reactive path's state: lane, losses on the way to a send,
// route survival and submission paths.
type Execution struct {
	Dispatch *DispatchView `json:"dispatch,omitempty"`
	Skips    []SkipCount   `json:"skips"`
	Survival *Survival     `json:"survival,omitempty"`
	Paths    []PathStats   `json:"paths"`
}

// Execution reports skips since sinceMS and path timings of the last day.
func (s *Store) Execution(ctx context.Context, instance string, sinceMS int64, now time.Time) (Execution, error) {
	result := Execution{Skips: []SkipCount{}, Paths: []PathStats{}}
	var body string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM reactive_latest WHERE instance=? AND type='dispatch_state'`, instance).Scan(&body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if err == nil {
		result.Dispatch = &DispatchView{}
		if err := json.Unmarshal([]byte(body), result.Dispatch); err != nil {
			return result, fmt.Errorf("decode stored dispatch state: %w", err)
		}
	}
	var atMS int64
	err = s.db.QueryRowContext(ctx, `SELECT at_ms, body FROM reactive_latest WHERE instance=? AND type='route_survival'`, instance).Scan(&atMS, &body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if err == nil {
		result.Survival = &Survival{AtMS: atMS}
		if err := json.Unmarshal([]byte(body), &result.Survival.Rows); err != nil {
			return result, fmt.Errorf("decode stored survival: %w", err)
		}
		slices.SortFunc(result.Survival.Rows, func(a, b SurvivalRow) int { return a.Hops - b.Hops })
	}
	rows, err := s.db.QueryContext(ctx, `SELECT reason, SUM(count) FROM reactive_skips WHERE instance=? AND to_ms>=? GROUP BY reason ORDER BY 2 DESC, reason`, instance, sinceMS)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var skip SkipCount
		if err := rows.Scan(&skip.Reason, &skip.Count); err != nil {
			return result, err
		}
		result.Skips = append(result.Skips, skip)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	result.Paths, err = s.pathStats(ctx, instance, now.Add(-24*time.Hour).UnixMilli())
	return result, err
}

func (s *Store) pathStats(ctx context.Context, instance string, sinceMS int64) ([]PathStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT paths FROM sends WHERE instance=? AND sent_ms>=? AND paths<>''`, instance, sinceMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	elapsed := map[string][]int64{}
	accepted := map[string]int{}
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var paths []PathResult
		if err := json.Unmarshal([]byte(encoded), &paths); err != nil {
			return nil, fmt.Errorf("decode stored paths: %w", err)
		}
		for _, path := range paths {
			elapsed[path.Name] = append(elapsed[path.Name], path.ElapsedMS)
			if path.Accepted {
				accepted[path.Name]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	stats := []PathStats{}
	for name, times := range elapsed {
		slices.Sort(times)
		stats = append(stats, PathStats{Name: name, Sends: len(times), Accepted: accepted[name], P50MS: rank(times, 0.5), P90MS: rank(times, 0.9), MaxMS: times[len(times)-1]})
	}
	slices.SortFunc(stats, func(a, b PathStats) int { return int(a.P50MS - b.P50MS) })
	return stats, nil
}

// rank is the nearest-rank quantile of sorted values.
func rank(sorted []int64, q float64) int64 {
	index := int(q*float64(len(sorted))+0.999999) - 1
	return sorted[min(max(index, 0), len(sorted)-1)]
}

// RivalStats summarizes one competing sender over a window.
type RivalStats struct {
	Sender       string  `json:"sender"`
	Transactions int     `json:"transactions"`
	Succeeded    int     `json:"succeeded"`
	AverageTip   float64 `json:"average_tip"`
	MaxTip       float64 `json:"max_tip"`
	AverageFrame float64 `json:"average_frame"`
	LastMS       int64   `json:"last_ms"`
}

type Competition struct {
	Transactions int          `json:"transactions"`
	Senders      int          `json:"senders"`
	Rivals       []RivalStats `json:"rivals"`
}

func (s *Store) Competition(ctx context.Context, instance string, sinceMS int64, limit int) (Competition, error) {
	result := Competition{Rivals: []RivalStats{}}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT sender) FROM rivals WHERE instance=? AND received_ms>=?`, instance, sinceMS).Scan(&result.Transactions, &result.Senders); err != nil {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sender, COUNT(*), SUM(success), AVG(tip_value), MAX(tip_value), AVG(frame), MAX(received_ms)
		FROM rivals WHERE instance=? AND received_ms>=? GROUP BY sender ORDER BY 2 DESC, sender LIMIT ?`, instance, sinceMS, limit)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var rival RivalStats
		if err := rows.Scan(&rival.Sender, &rival.Transactions, &rival.Succeeded, &rival.AverageTip, &rival.MaxTip, &rival.AverageFrame, &rival.LastMS); err != nil {
			return result, err
		}
		result.Rivals = append(result.Rivals, rival)
	}
	return result, rows.Err()
}

// Asset is a start asset's display unit as the bot read it from the token.
type Asset struct {
	Token    string `json:"token"`
	Symbol   string `json:"symbol"`
	Decimals int    `json:"decimals"`
}

func (s *Store) Assets(ctx context.Context, instance string) ([]Asset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token, symbol, decimals FROM assets WHERE instance=? ORDER BY token`, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := []Asset{}
	for rows.Next() {
		var asset Asset
		if err := rows.Scan(&asset.Token, &asset.Symbol, &asset.Decimals); err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}
