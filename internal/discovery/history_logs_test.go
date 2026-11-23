package discovery

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// Real OP Mainnet ETH/OP pool initialized at block 135758395 with a hook.
var opV4Manager = domain.Address{0x9a, 0x13, 0xf9, 0x8c, 0xb9, 0x87, 0x69, 0x4c, 0x9f, 0x08, 0x6b, 0x1f, 0x5e, 0xb9, 0x90, 0xee, 0xa8, 0x26, 0x4e, 0xc3}

// hookedOPInitialize decodes the recorded explorer getLogs reply into the RPC
// log shape that an archive eth_getLogs provider returns.
func hookedOPInitialize(t *testing.T) (domain.Hash, rpc.Log) {
	t.Helper()
	raw, err := os.ReadFile("testdata/v4_initialize_getlogs.json")
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Result []struct {
			Address, BlockNumber, Data string
			Topics                     []string
		}
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Result) != 1 {
		t.Fatal("fixture", err)
	}
	item := page.Result[0]
	log := rpc.Log{Address: opV4Manager}
	if log.BlockNumber, err = strconv.ParseUint(strings.TrimPrefix(item.BlockNumber, "0x"), 16, 64); err != nil {
		t.Fatal(err)
	}
	if log.Data, err = hex.DecodeString(strings.TrimPrefix(item.Data, "0x")); err != nil {
		t.Fatal(err)
	}
	for _, text := range item.Topics {
		topic, err := domain.ParseHash(text)
		if err != nil {
			t.Fatal(err)
		}
		log.Topics = append(log.Topics, topic)
	}
	return log.Topics[1], log
}

func v4Options(keys ...domain.PoolKey) ProbeOptions {
	activity := map[domain.PoolKey]Activity{}
	for _, key := range keys {
		activity[key] = Activity{Swaps: 5}
	}
	return ProbeOptions{Emitters: []Emitter{{SourceUniswapV4, opV4Manager}}, Activity: activity, Seen: map[domain.PoolKey]bool{}, Limit: 4, Anchor: domain.BlockRef{Number: 159000000}, Cooldown: 1}
}

func TestHistoryLogsResolveOldV4PoolByID(t *testing.T) {
	id, log := hookedOPInitialize(t)
	logs := newFakeLogs()
	logs.add(log)
	hooked := domain.PoolKey{Address: opV4Manager, Kind: domain.PoolKindV4, PoolID: id}
	missing := domain.PoolKey{Address: opV4Manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{1}}
	probe := ActiveProbe{}
	rows, err := probe.RunWithLogs(t.Context(), nil, HistoryLogs{Source: logs}, v4Options(hooked, missing))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Block != 135758395 || rows[0].Spec.PoolID != id || rows[0].Spec.Hooks == (domain.Address{}) {
		t.Fatal("old Initialize was not resolved", rows)
	}
	if len(logs.filters) != 1 || logs.filters[0].FromBlock != 0 || len(logs.filters[0].Topics[1]) != 2 {
		t.Fatal("IDs were not batched into one full-history request", logs.filters)
	}
	reasons := map[domain.Hash]string{}
	for _, row := range probe.Report {
		reasons[row.Pool.PoolID] = row.Reason
	}
	if reasons[id] != "unsupported_v4_hooks" || reasons[missing.PoolID] != "v4_initialize_not_found" {
		t.Fatal("unexpected assessments", reasons)
	}
}

func TestHistoryLogsOutageKeepsDiscoveryRoundAndHidesEndpoint(t *testing.T) {
	logs := newFakeLogs()
	logs.err = &url.Error{Op: "Post", URL: "https://example.invalid/v2/secret-key", Err: errors.New("connection reset")}
	key := domain.PoolKey{Address: opV4Manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{1}}
	options := v4Options(key)
	probe := ActiveProbe{}
	for round := 1; round <= 2; round++ {
		options.Anchor.Number++
		rows, err := probe.RunWithLogs(t.Context(), nil, HistoryLogs{Source: logs}, options)
		if err != nil || len(rows) != 0 {
			t.Fatal("history outage aborted discovery", rows, err)
		}
		if len(logs.filters) != round || len(probe.Report) != 1 || probe.Report[0].Reason != "v4_initialize_source_unavailable" {
			t.Fatal("outage was not retried once per round", logs.filters, probe.Report)
		}
	}
	_, err := HistoryLogs{Source: logs}.GetLogs(t.Context(), rpc.LogFilter{})
	if !errors.Is(err, ErrHistoryLogsUnavailable) || strings.Contains(err.Error(), "secret-key") {
		t.Fatal("history error should name the cause without the endpoint", err)
	}
}

func TestLoadedV4PoolIsNotLookedUpAgain(t *testing.T) {
	logs := newFakeLogs()
	loaded := domain.PoolKey{Address: opV4Manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{1}}
	options := v4Options(loaded)
	options.Known = []domain.PoolKey{loaded}
	probe := ActiveProbe{}
	rows, err := probe.RunWithLogs(t.Context(), nil, HistoryLogs{Source: logs}, options)
	if err != nil || len(rows) != 0 || len(logs.filters) != 0 {
		t.Fatal("loaded V4 pool was looked up and could be admitted twice", rows, err, logs.filters)
	}
}

func TestV4RangeRejectionIsNotRetriedEveryRound(t *testing.T) {
	logs := newFakeLogs()
	logs.err = &rpc.RPCError{Code: -32602, Message: "eth_getLogs is limited to 5000 block range"}
	key := domain.PoolKey{Address: opV4Manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{1}}
	options := v4Options(key)
	probe := ActiveProbe{}
	for i := 0; i < 3; i++ {
		options.Anchor.Number++
		if _, err := probe.RunWithLogs(t.Context(), nil, logs, options); err != nil {
			t.Fatal(err)
		}
	}
	if len(logs.filters) != 1 || len(probe.Report) != 1 || probe.Report[0].Reason != "historical_log_range_unsupported" {
		t.Fatal("repeated unsupported paid query", logs.filters, probe.Report)
	}
}
