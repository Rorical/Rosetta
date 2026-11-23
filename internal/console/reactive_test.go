package console

import (
	"encoding/json"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// reactiveFixture encodes records the way the bot's reactive writer does: the
// exec structs as they are, wrapped in the same maps where the bot uses maps.
type reactiveFixture struct {
	t                  *testing.T
	asset              domain.Address
	pools              []domain.PoolKey
	won, lost          domain.Hash
	triggerNS, landNS  int64
	triggerBlock       uint64
	reactive, competed strings.Builder
}

func newReactiveFixture(t *testing.T) *reactiveFixture {
	return &reactiveFixture{
		t: t, asset: domain.Address{0x42}, won: domain.Hash{0xa1}, lost: domain.Hash{0xb2}, triggerBlock: 100,
		pools:     []domain.PoolKey{{Address: domain.Address{0xaa}, Kind: domain.PoolKindV3}, {Address: domain.Address{0xbb}, Kind: domain.PoolKindV4, PoolID: domain.Hash{0xcc}}, {Address: domain.Address{0xdd}, Kind: domain.PoolKindVelodromeV2}},
		triggerNS: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC).UnixNano(),
	}
}

func (f *reactiveFixture) line(builder *strings.Builder, value any) {
	f.t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	builder.Write(encoded)
	builder.WriteByte('\n')
}

func (f *reactiveFixture) intent(id byte) execution.ReactiveIntent {
	steps := make([]execution.ExecutorStep, 0, len(f.pools))
	for _, pool := range f.pools {
		steps = append(steps, execution.ExecutorStep{Pool: pool})
	}
	return execution.ReactiveIntent{
		ID: domain.Hash{id}, Version: domain.StateVersion{Source: domain.SourceFast, BlockNumber: f.triggerBlock},
		Route: execution.ExecutorPlan{Asset: f.asset, Amount: big.NewInt(5_000_000_000_000_000_000), Steps: steps},
		Call:  execution.ExecutorCall{CallParameters: execution.CallParameters{GasLimit: 900_000, TipCap: big.NewInt(120_000)}},
		// Two asset units per wei.
		QuotedGross: big.NewInt(40_000_000_000_000), Budget: big.NewInt(3_000_000_000), ExpectedNet: big.NewInt(18_985_454_743_139_370),
		Conversion: execution.FeeConversion{Numerator: big.NewInt(2), Denominator: big.NewInt(1)},
		Bid:        &execution.BidDecision{ModelTip: big.NewInt(110_000), Wins: 31, Samples: 50, Applied: false}, CreatedMS: f.triggerNS / 1e6,
	}
}

func (f *reactiveFixture) send(hash domain.Hash, intent execution.ReactiveIntent, offsetMS int64) {
	started := f.triggerNS + offsetMS*1e6
	submission := &execution.SubmissionTiming{Hash: hash, StartedAtNS: started, ReturnedAtNS: started + 31e6, Acknowledged: true}
	f.line(&f.reactive, map[string]any{"type": "reactive_intent", "mode": "live", "action": "submit", "intent": intent, "transaction": hash, "submission": submission,
		"timing": map[string]any{"received_ns": f.triggerNS, "quoted_ns": f.triggerNS + 2e6, "consumed_ns": f.triggerNS + 3e6, "build_start_ns": f.triggerNS + 3e6, "sealed_ns": started - 1e6, "execute_ns": started, "lane": "fast"}})
}

func (f *reactiveFixture) observe(hash domain.Hash, block, frame, index uint64, success bool) {
	latency := int64(410e6)
	f.line(&f.reactive, &execution.ExecutionObservation{Type: "execution_observed", Hash: hash, Position: execution.ExecutionPosition{Stream: "flashblocks", BlockNumber: block, FrameIndex: frame, TransactionIndex: index, ReceivedAtNS: f.triggerNS + 600e6}, SendToObservedNS: &latency, Success: success})
}

func (f *reactiveFixture) settle(hash domain.Hash, intent execution.ReactiveIntent, block uint64, success bool, profit, net int64) {
	f.line(&f.reactive, map[string]any{"type": "settlement", "receipt": &execution.Settlement{
		Intent: &intent, Hash: hash, IntentID: intent.ID, Block: domain.BlockRef{Number: block, Timestamp: uint64(f.triggerNS/1e9) + 2}, Success: success, Asset: f.asset,
		Profit: big.NewInt(profit), LoanFee: new(big.Int), Net: big.NewInt(net), Basis: "canonical_receipt; admission_conversion",
		Fees: &domain.ReceiptFees{Family: domain.FamilyOPStack, GasUsed: 412_000, Execution: big.NewInt(900), L1: big.NewInt(100), Total: big.NewInt(1000)},
	}, "feed_reconciliation": "matched"})
}

func (f *reactiveFixture) rival(from domain.Address, block, frame, index uint64, own bool, pools ...domain.PoolKey) {
	f.line(&f.competed, map[string]any{"type": "competitor_tx", "block": block, "frame": frame, "tx_index": index, "hash": domain.Hash{byte(index)}, "from": from, "own": own,
		"tip_per_gas": big.NewInt(int64(frame) * 1000), "fee_cap": big.NewInt(1e9), "base_fee": big.NewInt(1e6), "gas_limit": 500000, "success": true, "pools": pools,
		"received_at_ns": f.triggerNS + int64(frame)*200e6 + int64(block-f.triggerBlock)*2e9})
}

// write builds a day of activity: one winning send, one send a rival beat.
func (f *reactiveFixture) write() {
	won, lost := f.intent(1), f.intent(2)
	f.line(&f.reactive, map[string]any{"type": "reactive_assets", "at_ms": f.triggerNS / 1e6, "assets": []domain.Token{{Address: f.asset, Symbol: "OP", Decimals: 18}}})
	f.send(f.won, won, 40)
	f.line(&f.reactive, map[string]any{"type": "submission_paths", "transaction": f.won, "at_ms": f.triggerNS / 1e6,
		"paths": []execution.PathResult{{Name: "primary", ElapsedMS: 31, Accepted: true}, {Name: "fanout.example", ElapsedMS: 58, Accepted: false, Error: "rpc -32000: already known"}}})
	f.observe(f.won, 101, 3, 7, true)
	f.settle(f.won, won, 101, true, 40_000_000_000_000, 37_000_000_000_000)
	f.send(f.lost, lost, 60)
	f.observe(f.lost, 102, 2, 5, false)
	f.settle(f.lost, lost, 102, false, 0, -2000)
	f.line(&f.reactive, struct {
		Type string `json:"type"`
		AtMS int64  `json:"at_ms"`
		execution.DispatchStatus
	}{"dispatch_state", f.triggerNS / 1e6, execution.DispatchStatus{HaltedReason: "fee overrun", NextNonce: 9, Pending: &f.lost, Variants: 1, SpentToday: big.NewInt(4_000_000_000), DailyCap: big.NewInt(100_000_000_000_000), PerTransaction: big.NewInt(10_000_000_000_000)}})
	f.line(&f.reactive, map[string]any{"type": "route_survival", "at_ms": f.triggerNS / 1e6, "survived_of_total_by_hops": map[int][2]int{4: {9, 10}, 2: {1, 12}}})
	f.line(&f.reactive, map[string]any{"type": "reactive_skips", "from_ms": f.triggerNS / 1e6, "to_ms": f.triggerNS/1e6 + 60_000, "reasons": map[string]uint64{"below_build_rank": 7, "canonical_lag": 2}})
	f.line(&f.reactive, map[string]any{"type": "execution", "status": "success"})

	rival, other := domain.Address{0x77}, domain.Address{0x88}
	f.rival(rival, 102, 1, 3, false, f.pools[1], domain.PoolKey{Address: domain.Address{0x01}, Kind: domain.PoolKindV2})
	f.rival(other, 102, 0, 1, false, domain.PoolKey{Address: domain.Address{0x02}, Kind: domain.PoolKindV2}, domain.PoolKey{Address: domain.Address{0x03}, Kind: domain.PoolKindV2})
	f.rival(domain.Address{0x99}, 102, 1, 2, true, f.pools...)
	f.line(&f.competed, map[string]any{"type": "competitor_dropped", "count": 3})
}

func importFixture(t *testing.T, store *Store, f *reactiveFixture) {
	t.Helper()
	dir := t.TempDir()
	for kind, text := range map[string]string{kindReactive: f.reactive.String(), kindCompetition: f.competed.String()} {
		path := filepath.Join(dir, kind+".jsonl")
		appendFile(t, path, text)
		if err := store.trackFile(t.Context(), tailFile{Path: path, Instance: "one", RunID: "run-1", Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	tailer := &Tailer{store: store, instance: "one"}
	if err := tailer.Pass(t.Context(), "run-1"); err != nil {
		t.Fatal(err)
	}
}

func TestReactiveRecordsFromBotEncoders(t *testing.T) {
	store := testStore(t)
	f := newReactiveFixture(t)
	f.write()
	importFixture(t, store, f)
	now := time.Unix(0, f.triggerNS).Add(time.Hour)

	sends, err := store.Sends(t.Context(), "one", "", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sends) != 2 || sends[0].Hash != f.lost.Hex() || sends[1].Hash != f.won.Hex() {
		t.Fatalf("sends: %+v", sends)
	}
	lost, won := sends[0], sends[1]
	if won.Outcome != "success" || won.Input != "5000000000000000000" || won.ExpectedNet != "18985454743139370" || won.Tip != "120000" || won.TriggerBlock != 100 || won.TriggerSource != "fast" || won.Lane != "fast" {
		t.Fatalf("won intent: %+v", won)
	}
	if won.AckNS == nil || *won.AckNS != 31e6 || won.SentMS != f.triggerNS/1e6+40 || won.ExecuteNS-won.ReceivedNS != 40e6 {
		t.Fatalf("won timing: %+v", won)
	}
	if len(won.Route) != 3 || won.Route[1] != (poolRef{domain.Hash{0xcc}.Hex(), "V4"}) || won.Route[2].Kind != "Velodrome V2" {
		t.Fatalf("route: %+v", won.Route)
	}
	if won.Bid == nil || won.Bid.ModelTip != "110000" || won.Bid.Wins != 31 || len(won.Paths) != 2 || won.Paths[1].Error == "" {
		t.Fatalf("bid or paths: %+v %+v", won.Bid, won.Paths)
	}
	if won.Landing == nil || won.Landing.Frame != 3 || won.Landing.Index != 7 || *won.Landing.SendToFeedNS != 410e6 || won.Rival != nil {
		t.Fatalf("landing: %+v", won.Landing)
	}
	// Profit is 2e13 wei at two asset units per wei, less the 1000 wei fee.
	if won.Settled == nil || won.Settled.Fee != "1000" || won.Settled.L1Fee != "100" || won.Settled.GasUsed != 412_000 || *won.Settled.NetNative != 20_000_000_000_000-1000 {
		t.Fatalf("settlement: %+v", won.Settled)
	}
	// The rival in frame 1 touched our V4 pool before our frame-2 revert; our own
	// sighting and an unrelated route do not count.
	if lost.Outcome != "reverted" || lost.Rival == nil || lost.Rival.Sender != (domain.Address{0x77}).Hex() || lost.Rival.Frame != 1 || lost.Rival.Tip != "1000" {
		t.Fatalf("lost: %+v rival %+v", lost, lost.Rival)
	}
	if only, _ := store.Sends(t.Context(), "one", "success", 10, now); len(only) != 1 || only[0].Hash != f.won.Hex() {
		t.Fatalf("outcome filter: %+v", only)
	}

	days, err := store.PnL(t.Context(), "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Day != "2026-11-02" || days[0].Asset != f.asset.Hex() || days[0].Settled != 2 || days[0].Succeeded != 1 || days[0].Reverted != 1 ||
		days[0].Fee != 2000 || days[0].Net != 37_000_000_000_000-2000 || days[0].NetNative != 20_000_000_000_000-2000 {
		t.Fatalf("pnl: %+v", days)
	}

	execution, err := store.Execution(t.Context(), "one", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := execution.Dispatch
	if dispatch == nil || dispatch.HaltedReason != "fee overrun" || dispatch.Pending != f.lost.Hex() || dispatch.SpentToday != "4000000000" || dispatch.NextNonce != 9 {
		t.Fatalf("dispatch: %+v", dispatch)
	}
	if execution.Survival == nil || len(execution.Survival.Rows) != 2 || execution.Survival.Rows[0] != (SurvivalRow{Hops: 2, Survived: 1, Total: 12}) {
		t.Fatalf("survival: %+v", execution.Survival)
	}
	if len(execution.Skips) != 2 || execution.Skips[0] != (SkipCount{"below_build_rank", 7}) {
		t.Fatalf("skips: %+v", execution.Skips)
	}
	if len(execution.Paths) != 2 || execution.Paths[0] != (PathStats{Name: "primary", Sends: 1, Accepted: 1, P50MS: 31, P90MS: 31, MaxMS: 31}) {
		t.Fatalf("paths: %+v", execution.Paths)
	}
	if later, _ := store.Execution(t.Context(), "one", f.triggerNS/1e6+61_000, now); len(later.Skips) != 0 {
		t.Fatalf("skips before the run counted: %+v", later.Skips)
	}

	competition, err := store.Competition(t.Context(), "one", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if competition.Transactions != 2 || competition.Senders != 2 || competition.Rivals[0].Transactions != 1 {
		t.Fatalf("competition: %+v", competition)
	}
	assets, err := store.Assets(t.Context(), "one")
	if err != nil || len(assets) != 1 || assets[0] != (Asset{Token: f.asset.Hex(), Symbol: "OP", Decimals: 18}) {
		t.Fatalf("assets: %+v %v", assets, err)
	}
}

// The bot books a feed sighting before reading the receipt. The send shows its
// outcome at once with fees pending; the receipt's record fills them in, and a
// canonical block that chose another variant voids the booking.
func TestFeedSettlementShowsOutcomeBeforeFees(t *testing.T) {
	store := testStore(t)
	f := newReactiveFixture(t)
	won, lost := f.intent(1), f.intent(2)
	position := execution.ExecutionPosition{Stream: "flashblocks", BlockNumber: 101, FrameIndex: 3, TransactionIndex: 7, ReceivedAtNS: f.triggerNS + 600e6}
	feed := func(hash domain.Hash, intent execution.ReactiveIntent, status string) {
		f.line(&f.reactive, map[string]any{"type": "settlement", "receipt": &execution.Settlement{Intent: &intent, Hash: hash, IntentID: intent.ID, Block: domain.BlockRef{Number: 101},
			Position: &position, Success: true, Asset: f.asset, Profit: big.NewInt(40_000_000_000_000), LoanFee: new(big.Int), FeeStatus: status, Basis: "flashblocks_feed; admission_conversion"}})
	}
	f.send(f.won, won, 40)
	f.observe(f.won, 101, 3, 7, true)
	feed(f.won, won, execution.FeesPending)
	f.send(f.lost, lost, 60)
	feed(f.lost, lost, execution.FeesUnknown)
	importFixture(t, store, f)
	now := time.Unix(0, f.triggerNS).Add(time.Minute)
	sends, err := store.Sends(t.Context(), "one", "", 10, now)
	if err != nil || len(sends) != 2 {
		t.Fatal(sends, err)
	}
	pending, unknown := sends[1].Settled, sends[0].Settled
	if sends[1].Outcome != "success" || pending == nil || pending.FeeStatus != "pending" || pending.Fee != "" || pending.Net != "" || pending.NetNative != nil || pending.AtMS != (f.triggerNS+600e6)/1e6 {
		t.Fatalf("feed settlement: %+v %+v", sends[1], pending)
	}
	if unknown == nil || unknown.FeeStatus != "unknown" || unknown.Fee != "" {
		t.Fatalf("unknown fee: %+v", unknown)
	}
	if days, _ := store.PnL(t.Context(), "one"); len(days) != 1 || days[0].Settled != 2 || days[0].Succeeded != 2 || days[0].Fee != 0 || days[0].Net != 0 || days[0].Profit != 80_000_000_000_000 {
		t.Fatalf("pnl before fees: %+v", days)
	}

	f.reactive.Reset()
	f.settle(f.won, won, 101, true, 40_000_000_000_000, 37_000_000_000_000)
	f.line(&f.reactive, map[string]any{"type": "settlement_voided", "transaction": f.lost, "replaced_by": domain.Hash{0xc3}})
	importFixture(t, store, f)
	sends, err = store.Sends(t.Context(), "one", "", 10, now)
	if err != nil || len(sends) != 2 {
		t.Fatal(sends, err)
	}
	if paid := sends[1].Settled; paid == nil || paid.FeeStatus != "" || paid.Fee != "1000" || paid.Net != "37000000000000" || paid.NetNative == nil {
		t.Fatalf("paid settlement: %+v", paid)
	}
	if sends[0].Settled != nil || sends[0].Outcome != "pending" {
		t.Fatalf("voided settlement still shown: %+v", sends[0])
	}
}

// A database written before feed settlement gains the fee status column.
func TestStoreAddsFeeStatusToAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.db")
	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(t.Context(), `ALTER TABLE sends DROP COLUMN fee_status`); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Sends(t.Context(), "one", "", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSendWithoutSightingBecomesUnlanded(t *testing.T) {
	store := testStore(t)
	f := newReactiveFixture(t)
	f.send(f.won, f.intent(1), 40)
	importFixture(t, store, f)
	sent := time.Unix(0, f.triggerNS)
	if sends, _ := store.Sends(t.Context(), "one", "", 10, sent.Add(time.Second)); len(sends) != 1 || sends[0].Outcome != "pending" {
		t.Fatalf("fresh send: %+v", sends)
	}
	if sends, _ := store.Sends(t.Context(), "one", "unlanded", 10, sent.Add(landingGrace+time.Second)); len(sends) != 1 || sends[0].Outcome != "unlanded" {
		t.Fatalf("stale send: %+v", sends)
	}
}

// The bot appends to one reactive file across restarts. A newer run reopens the
// path at its old offset, and a replaced file is read from the start.
func TestAppendedOutputContinuesAcrossRuns(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "reactive.jsonl")
	appendFile(t, path, skipLine(t, "a", 1))
	if err := store.trackFile(ctx, tailFile{Path: path, Instance: "one", RunID: "run-1", Kind: kindReactive}); err != nil {
		t.Fatal(err)
	}
	tailer := &Tailer{store: store, instance: "one"}
	if err := tailer.Pass(ctx, "run-2"); err != nil {
		t.Fatal(err)
	}
	if files, _ := store.openFiles(ctx, "one"); len(files) != 0 {
		t.Fatal("finished run kept its file open")
	}
	if err := store.trackFile(ctx, tailFile{Path: path, Instance: "one", RunID: "run-2", Kind: kindReactive}); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, skipLine(t, "a", 2))
	if err := tailer.Pass(ctx, "run-2"); err != nil {
		t.Fatal(err)
	}
	if counts := skipsStored(t, store); counts["a"] != 3 {
		t.Fatalf("reopened file: %+v", counts)
	}
	if err := os.WriteFile(path, []byte(skipLine(t, "b", 5)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tailer.Pass(ctx, "run-2"); err != nil {
		t.Fatal(err)
	}
	if counts := skipsStored(t, store); counts["b"] != 5 || counts["a"] != 3 {
		t.Fatalf("replaced file: %+v", counts)
	}
}

func TestPruneBoundsRivalsAndSkips(t *testing.T) {
	store := testStore(t)
	file := tailFile{Instance: "one", RunID: "r", Path: "p"}
	records := &batch{skips: []skipRow{{ToMS: 10, Reason: "old", Count: 1}, {ToMS: 100, Reason: "new", Count: 1}}}
	for i := range 5 {
		records.rivals = append(records.rivals, rivalRow{Block: uint64(i), Hash: domain.Hash{byte(i)}.Hex(), Sender: "s", Tip: "1", ReceivedMS: int64(i)})
	}
	if err := store.commitBatch(t.Context(), file, 0, records); err != nil {
		t.Fatal(err)
	}
	if err := store.prune(t.Context(), 2, 50); err != nil {
		t.Fatal(err)
	}
	competition, err := store.Competition(t.Context(), "one", 0, 10)
	if err != nil || competition.Transactions != 2 {
		t.Fatalf("rivals kept: %+v %v", competition, err)
	}
	if counts := skipsStored(t, store); len(counts) != 1 || counts["new"] != 1 {
		t.Fatalf("skips kept: %+v", counts)
	}
}

func TestReactiveAPIValidatesQueries(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t)
	f := newReactiveFixture(t)
	f.write()
	importFixture(t, h.store, f)
	for path, code := range map[string]int{
		"/api/instances/one/sends?outcome=bogus":    http.StatusBadRequest,
		"/api/instances/one/execution":              http.StatusBadRequest,
		"/api/instances/one/competition?range=99y":  http.StatusBadRequest,
		"/api/instances/nope/sends":                 http.StatusNotFound,
		"/api/instances/one/sends?outcome=reverted": http.StatusOK,
		"/api/instances/one/execution?since=0":      http.StatusOK,
		"/api/instances/one/competition?range=7d":   http.StatusOK,
		"/api/instances/one/pnl":                    http.StatusOK,
		"/api/instances/one/assets":                 http.StatusOK,
	} {
		if response := h.do(t, http.MethodGet, path, "", nil); response.StatusCode != code {
			t.Errorf("%s: %d, want %d", path, response.StatusCode, code)
		}
	}
	var sends []Send
	if err := json.NewDecoder(h.do(t, http.MethodGet, "/api/instances/one/sends?outcome=reverted", "", nil).Body).Decode(&sends); err != nil || len(sends) != 1 || sends[0].Rival == nil {
		t.Fatalf("reverted sends: %+v %v", sends, err)
	}
}

func TestQuantileChartsNameSeriesByLabel(t *testing.T) {
	var query string
	prometheus := fakePrometheus(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"series":"p90"},"values":[]},{"metric":{"series":"p50"},"values":[]}]}}`))
	})
	history, err := prometheus.Chart(t.Context(), "one", "send_to_feed", "1h", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `stage="reactive_send_to_feed"`) || strings.Count(query, `rosetta_instance="one"`) != 2 {
		t.Fatalf("query %s", query)
	}
	if len(history.Series) != 2 || history.Series[0].Name != "p50" || history.Series[1].Name != "p90" {
		t.Fatalf("series: %+v", history.Series)
	}
}

func TestDisplayedSendAgeIncludesDispatcherAndSigning(t *testing.T) {
	var row reactiveLine
	raw, err := json.Marshal(map[string]any{"transaction": domain.Hash{1}, "intent": map[string]any{"ID": domain.Hash{2}}, "timing": map[string]int64{"received_ns": 1000000000, "execute_ns": 1020000000}, "submission": map[string]int64{"StartedAtNS": 1040000000, "ReturnedAtNS": 1090000000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatal(err)
	}
	send, ok := intentSend(row)
	if !ok || send.ExecuteNS-send.ReceivedNS != 40000000 || send.AckNS == nil || *send.AckNS != 50000000 {
		t.Fatal(send, ok)
	}
}
