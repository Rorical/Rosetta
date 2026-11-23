package console

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Charts are fixed on the server. The browser picks a name and a range; it
// never supplies PromQL, so the console cannot be turned into a query proxy.
// %[1]s is the instance selector. A series is named by its "series" label,
// else its source.
var charts = map[string]string{
	"messages":        `sum by (source) (rate(rosetta_pipeline_events_total{%[1]s,event="messages"}[1m]))`,
	"candidates":      `sum by (source) (rate(rosetta_pipeline_events_total{%[1]s,event="candidates"}[5m]))`,
	"errors":          `sum by (source) (rate(rosetta_pipeline_events_total{%[1]s,event=~".*_errors|block_mismatch"}[5m]))`,
	"head_age":        `max by (source) (time() - (rosetta_pipeline_state{%[1]s,gauge="head_timestamp_seconds"} > 0))`,
	"sync_lag":        `max by (source) (rosetta_pipeline_state{%[1]s,gauge="sync_lag_blocks"})`,
	"rpc_failures":    `sum by (source) (rate(rosetta_pipeline_events_total{%[1]s,event=~"rpc_http_errors|rpc_throttled"}[5m]))`,
	"cpu":             `rate(rosetta_process_cpu_seconds_total{%[1]s}[1m])`,
	"memory":          `rosetta_process_memory_bytes{%[1]s}`,
	"sends":           `label_replace(sum by (event) (increase(rosetta_pipeline_events_total{%[1]s,event=~"reactive_(sent|landed|succeeded|reverted)"}[1h])), "series", "$1", "event", "reactive_(.*)")`,
	"pools":           `max(rosetta_pipeline_state{%[1]s,source="standard",gauge="pools"})`,
	"wallet":          `max(rosetta_pipeline_state{%[1]s,source="standard",gauge="wallet_native_gwei"}) / 1e9`,
	"search_queue":    stageQuantiles("search_queue"),
	"search_evaluate": stageQuantiles("search_evaluate"),
	"first_candidate": stageQuantiles("first_candidate"),
	"trigger_to_send": stageQuantiles("reactive_trigger_to_send"),
	"send_ack":        stageQuantiles("reactive_send_ack"),
	"send_to_feed":    stageQuantiles("reactive_send_to_feed"),
}

// stageQuantiles draws one stage's p50 and p90 across sources.
func stageQuantiles(stage string) string {
	quantile := func(q, name string) string {
		return `label_replace(histogram_quantile(` + q + `, sum by (le) (rate(rosetta_pipeline_duration_seconds_bucket{%[1]s,stage="` + stage + `"}[5m]))), "series", "` + name + `", "", "")`
	}
	return quantile("0.5", "p50") + " or " + quantile("0.9", "p90")
}

var chartRanges = map[string]time.Duration{
	"15m": 15 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
}

const chartPoints = 240

type Prometheus struct {
	base string
	http *http.Client
}

// NewPrometheus returns nil for an empty URL. History and alert endpoints then
// report that Prometheus is not configured.
func NewPrometheus(base string) *Prometheus {
	if base == "" {
		return nil
	}
	return &Prometheus{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

func (p *Prometheus) get(ctx context.Context, path string, query url.Values, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	response, err := p.http.Do(request)
	if err != nil {
		return fmt.Errorf("prometheus unreachable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read prometheus response: %w", err)
	}
	var envelope struct {
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode prometheus response: %w", err)
	}
	if envelope.Status != "success" {
		return fmt.Errorf("prometheus query failed: %s", envelope.Error)
	}
	return json.Unmarshal(envelope.Data, target)
}

// Series is one line of a chart, aligned with History.Timestamps. Nil marks a
// gap so a chart breaks the line instead of drawing through missing scrapes.
type Series struct {
	Name   string     `json:"name"`
	Values []*float64 `json:"values"`
}

type History struct {
	Timestamps []int64  `json:"timestamps"`
	Series     []Series `json:"series"`
}

// Chart runs one fixed query over a window ending at now.
func (p *Prometheus) Chart(ctx context.Context, instance, chart, window string, now time.Time) (History, error) {
	template, ok := charts[chart]
	if !ok {
		return History{}, errUnknownChart
	}
	span, ok := chartRanges[window]
	if !ok {
		return History{}, errUnknownChart
	}
	step := max(span/chartPoints, 5*time.Second)
	end := now.Truncate(step)
	start := end.Add(-span)
	selector := fmt.Sprintf("rosetta_instance=%q", instance)
	query := url.Values{
		"query": {fmt.Sprintf(template, selector)},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	var data struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	}
	if err := p.get(ctx, "/api/v1/query_range", query, &data); err != nil {
		return History{}, err
	}
	history := History{Timestamps: []int64{}, Series: []Series{}}
	for at := start; !at.After(end); at = at.Add(step) {
		history.Timestamps = append(history.Timestamps, at.Unix())
	}
	for _, result := range data.Result {
		series := Series{Name: cmp.Or(result.Metric["series"], result.Metric["source"], "process"), Values: make([]*float64, len(history.Timestamps))}
		for _, point := range result.Values {
			at, ok := point[0].(float64)
			text, isText := point[1].(string)
			if !ok || !isText {
				return History{}, fmt.Errorf("unexpected prometheus sample")
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}
			index := int((int64(at) - start.Unix()) / int64(step.Seconds()))
			if index >= 0 && index < len(series.Values) {
				series.Values[index] = &value
			}
		}
		history.Series = append(history.Series, series)
	}
	slices.SortFunc(history.Series, func(a, b Series) int { return strings.Compare(a.Name, b.Name) })
	return history, nil
}

type Alert struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	State    string `json:"state"`
	Instance string `json:"instance"`
	Summary  string `json:"summary"`
	ActiveAt string `json:"active_at"`
}

func (p *Prometheus) Alerts(ctx context.Context) ([]Alert, error) {
	var data struct {
		Alerts []struct {
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
			State       string            `json:"state"`
			ActiveAt    string            `json:"activeAt"`
		} `json:"alerts"`
	}
	if err := p.get(ctx, "/api/v1/alerts", url.Values{}, &data); err != nil {
		return nil, err
	}
	alerts := make([]Alert, 0, len(data.Alerts))
	for _, alert := range data.Alerts {
		alerts = append(alerts, Alert{
			Name: alert.Labels["alertname"], Severity: alert.Labels["severity"], State: alert.State,
			Instance: alert.Labels["rosetta_instance"], Summary: alert.Annotations["summary"], ActiveAt: alert.ActiveAt,
		})
	}
	return alerts, nil
}
