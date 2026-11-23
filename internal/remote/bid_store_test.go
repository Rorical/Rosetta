package remote

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// A restart that empties the in-memory estimator bids by the fallback share,
// which loses. A restart must bid as the process before it did, calibration
// included, as soon as it starts.
func TestBidHistorySurvivesARestart(t *testing.T) {
	h := newBidHistory()
	v4 := []domain.PoolKey{{Address: domain.Address{1}, Kind: domain.PoolKindV4, PoolID: domain.Hash{7}, Hooks: domain.Address{8}}, {Address: domain.Address{2}, Kind: domain.PoolKindV3}}
	for i := range 60 {
		pools := []domain.PoolKey{{Address: domain.Address{byte(20 + i%7)}, Kind: domain.PoolKindV2}, v4[i%2]}
		switch i % 4 {
		case 0:
			h.window(pools, 1_000_000, bidContest{from: rival2905, tip: 21_534}, bidContest{from: rivalD4AF, tip: 300_000})
		case 1:
			h.e.register(pools, 0, h.at)
			h.e.observe(rivalSighting{pools: pools, from: rivalA408}, h.at.Add(50*time.Millisecond))
			h.at = h.at.Add(time.Second)
		default:
			h.window(pools, 1_000_000)
		}
	}
	h.e.observeGas(527_812, 1_250_000)
	for i := range 10 {
		worth := []uint64{100_000, 1_000_000, 4_000_000}[i%3]
		h.e.settle(bidOutcomeOf(settlement(0, i%3 == 0, &execution.BidDecision{ModelTip: big.NewInt(1), Win: 0.7, WinRaw: 0.7, Worth: worth}), h.at))
		h.at = h.at.Add(2 * time.Second)
	}
	fresh := []domain.PoolKey{{Address: domain.Address{0x99}}}
	before, beforeFresh := h.decide(v4, 1_000_000), h.decide(fresh, 1_000_000)

	path := bidStorePath(filepath.Join(t.TempDir(), "reactive.jsonl"))
	saved := h.at.Add(time.Minute)
	if err := h.e.save(path, saved); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("save left %d files, want the store alone", len(entries))
	}

	restarted := &bidHistory{e: newBidEstimator(250*time.Millisecond, 550*time.Millisecond), at: h.at}
	restarted.e.load(path, saved.Add(time.Minute))
	after, afterFresh := restarted.decide(v4, 1_000_000), restarted.decide(fresh, 1_000_000)
	a, _ := json.Marshal([]*execution.BidDecision{before, beforeFresh})
	b, _ := json.Marshal([]*execution.BidDecision{after, afterFresh})
	if string(a) != string(b) || before.Shift == 0 || before.ShiftSlope == 0 {
		t.Fatalf("restart changed the decision:\n%s\n%s", a, b)
	}
	if restarted.e.expectedGas(1_250_000) != h.e.expectedGas(1_250_000) {
		t.Fatal("gas ratios were not restored")
	}

	stale := &bidHistory{e: newBidEstimator(250*time.Millisecond, 550*time.Millisecond), at: h.at}
	stale.e.load(path, saved.Add(7*time.Hour))
	if d := stale.decide(fresh, 1_000_000); d != nil || stale.e.calibration.intercept != 0 {
		t.Fatalf("history older than %s still decided: %+v", bidStoreMaxAge, d)
	}
}

// A format 1 file is migrated: its best bid per window becomes that
// window's contest, its first landers stay first, and its per-sender bid
// ratios are dropped.
func TestBidHistoryMigratesFormat1(t *testing.T) {
	raw := `{"format":1,"saved_ms":1800000000000,"pools":[{"a":"0x0100000000000000000000000000000000000000","k":1},{"a":"0x0200000000000000000000000000000000000000","k":1}],
	"samples":[{"t":1799999990000,"p":[0,1],"b":21534,"f":"0x2905b57c00000000000000000000000000000000","w":1000000},
	{"t":1799999991000,"p":[0],"b":18446744073709551615},
	{"t":1799999992000,"p":[1],"b":0,"w":50000},
	{"t":1799999993000,"p":[1],"b":900}],
	"gas":[0.42],"rivals":[{"f":"0x2905b57c00000000000000000000000000000000","t":1799999990000,"b":21534,"r":[1.5,0.7]}]}`
	path := filepath.Join(t.TempDir(), "reactive.jsonl.bids.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newBidEstimator(250*time.Millisecond, 550*time.Millisecond)
	e.load(path, time.UnixMilli(1_800_000_060_000))
	if len(e.windows) != 4 || len(e.gasRatios) != 0 {
		t.Fatalf("migrated %d windows and %d gas ratios", len(e.windows), len(e.gasRatios))
	}
	w := e.windows
	if len(w[0].contest) != 1 || w[0].contest[0] != (bidContest{from: rival2905, tip: 21_534}) || w[0].worth != 1_000_000 || len(w[0].pools) != 2 {
		t.Fatalf("contested window: %+v", w[0])
	}
	if !w[1].first || len(w[1].contest) != 0 || len(w[2].contest) != 0 || w[2].first || w[3].contest[0] != (bidContest{from: domain.Address{}, tip: 900}) {
		t.Fatalf("first, quiet and anonymous windows: %+v", w[1:])
	}
	// The migrated history is saved in the current format.
	if err := e.save(path, time.UnixMilli(1_800_000_060_000)); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(path)
	var file bidFile
	if err := json.Unmarshal(saved, &file); err != nil || file.Format != bidStoreFormat || len(file.Windows) != 4 || len(file.Samples) != 0 {
		t.Fatalf("resaved: %s", saved)
	}
}

// A format 2 file recorded outcomes without their worth: they load as
// outcomes of unknown size, which fit the intercept as before and leave the
// slope at zero, and the history is saved again in the current format; ambiguous gas samples are dropped.
func TestBidHistoryMigratesFormat2(t *testing.T) {
	raw := `{"format":2,"saved_ms":1800000000000,"pools":[{"a":"0x0100000000000000000000000000000000000000","k":1}],
	"windows":[{"t":1799999990000,"p":[0],"w":1000000,"c":[{"f":"0x2905b57c00000000000000000000000000000000","b":21534}]}],
	"gas":[0.42],"calibration":[{"t":1799999990000,"p":0.5,"y":true},{"t":1799999991000,"p":0.5,"y":true},{"t":1799999992000,"p":0.5,"y":true},{"t":1799999993000,"p":0.5}]}`
	path := filepath.Join(t.TempDir(), "reactive.jsonl.bids.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1_800_000_060_000)
	e := newBidEstimator(250*time.Millisecond, 550*time.Millisecond)
	e.load(path, now)
	c := e.calibration
	if len(e.windows) != 1 || len(c.points) != 4 || c.slope != 0 || c.intercept <= 0 || c.centerWorth() != 0 {
		t.Fatalf("migrated %d windows, %d outcomes, intercept %.3f slope %.3f", len(e.windows), len(c.points), c.intercept, c.slope)
	}
	if err := e.save(path, now); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(path)
	var file bidFile
	if err := json.Unmarshal(saved, &file); err != nil || file.Format != bidStoreFormat || bidStoreFormat != 5 || len(file.Calibration) != 4 {
		t.Fatalf("resaved: %s", saved)
	}
}

func TestCorruptBidHistoryStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reactive.jsonl.bids.json")
	for _, raw := range []string{
		`{"format":1,"samples":[{"t":1,"p":[3],"b":5}]}`,
		`{"format":2,"windows":[{"t":1,"p":[],"c":[{"f":"0x12","b":1}]}]}`,
		`{"format":2,"windows":[{"t":1,"p":[],"fb":"zz"}]}`,
		`{"format":2,"calibration":[{"t":1,"p":1.5}]}`,
		`{"format":1,"gas":[-1]}`,
		`{"format":9}`,
		`not json`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		e := newBidEstimator(time.Second, 2*time.Second)
		e.addWindow(bidWindow{worth: 7})
		e.load(path, time.Now())
		if len(e.windows) != 1 {
			t.Fatalf("%s: a corrupt file replaced the history", raw)
		}
	}
	missing := newBidEstimator(time.Second, 2*time.Second)
	missing.load(filepath.Join(t.TempDir(), "absent"), time.Now())
	if len(missing.windows) != 0 {
		t.Fatal("a first start is not empty")
	}
}
