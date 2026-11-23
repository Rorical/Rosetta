package remote

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func abiString(text string) []byte {
	raw := make([]byte, 96)
	raw[31] = 32
	raw[63] = byte(len(text))
	copy(raw[64:], text)
	return raw
}

func TestDecodeSymbolAcceptsStringAndBytes32Only(t *testing.T) {
	bytes32 := make([]byte, 32)
	copy(bytes32, "MKR")
	oversized := abiString("WETH")
	oversized[63] = 200
	for _, test := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"abi string", abiString("USDC.e"), "USDC.e"},
		{"bytes32", bytes32, "MKR"},
		{"length past data", oversized, ""},
		{"control characters", abiString("A\nB"), ""},
		{"too long", abiString(strings.Repeat("X", maxSymbol+1)), ""},
		{"short answer", []byte{1, 2, 3}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := decodeSymbol(test.raw); got != test.want {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestReactiveReportsFunnelAndDispatchState(t *testing.T) {
	r, event, _ := liveReactiveFixture(t)
	s := r.reactive
	s.metrics = &telemetry.Registry{}
	s.observations = make(chan any, 4)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(s.primary())
	if job == nil {
		t.Fatal("no intent")
	}
	result := s.executeJob(t.Context(), job, s.primary())
	hash, _ := result["transaction"].(domain.Hash)
	if hash == (domain.Hash{}) {
		t.Fatal(result)
	}
	r.observeReactiveExecution(hash, true, nil, execution.ExecutionPosition{BlockNumber: 10, FrameIndex: 1, ReceivedAtNS: time.Now().UnixNano()})
	s.observeSettlement(execution.SettlementUpdate{Settlement: &execution.Settlement{Hash: hash, Success: false, Intent: &job.intent}, First: true})

	var records []any
	write := func(value any) bool { records = append(records, value); return true }
	now := time.Now()
	if !s.publishDispatch(now, s.primary(), write) || !s.publishDispatch(now, s.primary(), write) {
		t.Fatal("write refused")
	}
	if len(records) != 1 {
		t.Fatalf("unchanged state rewritten: %d records", len(records))
	}
	encoded, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Type    string       `json:"type"`
		Pending *domain.Hash `json:"pending"`
		Spent   *big.Int     `json:"spent_today"`
		Cap     *big.Int     `json:"daily_cap"`
	}
	if err := json.Unmarshal(encoded, &state); err != nil || state.Type != "dispatch_state" || state.Pending == nil || *state.Pending != hash || state.Spent.Sign() <= 0 || state.Cap.Int64() != 10000000 {
		t.Fatalf("dispatch record %s: %v", encoded, err)
	}
	snapshot := s.metrics.Snapshot().Sources
	counters := snapshot["standard"].Counters
	for name, want := range map[string]uint64{"reactive_admitted": 1, "reactive_built": 1, "reactive_sent": 1, "reactive_landed": 1, "reactive_reverted": 1, "reactive_succeeded": 0} {
		if got := counters[name] + snapshot["fast"].Counters[name]; got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}
	gauges := snapshot["standard"].Gauges
	if gauges["dispatch_pending"] != 1 || gauges["dispatch_halted"] != 0 || gauges["dispatch_stopped"] != 0 {
		t.Fatalf("dispatch gauges: %+v", gauges)
	}
	if stage := snapshot["standard"].Stages["reactive_send_ack"]; stage.Count != 1 {
		t.Fatalf("send acknowledgement not timed: %+v", stage)
	}
}

func TestSubmissionPathRecordDropsWhenQueueIsFull(t *testing.T) {
	s := &reactiveRuntime{observations: make(chan any, 1), diagnostics: make(chan any, 1)}
	logged := 0
	record := s.pathRecorder(func(domain.Hash, []execution.PathResult) { logged++ })
	results := []execution.PathResult{{Name: "primary", ElapsedMS: 12, Accepted: true}}
	record(domain.Hash{1}, results)
	record(domain.Hash{2}, results)
	if logged != 2 || len(s.diagnostics) != 1 || s.failure.Load() != nil {
		t.Fatalf("logged=%d queued=%d", logged, len(s.diagnostics))
	}
	// Dropped diagnostics leave the evidence queue free.
	s.enqueueObservation(&execution.ExecutionObservation{})
	if len(s.observations) != 1 || s.failure.Load() != nil {
		t.Fatal("a full diagnostics queue cost an execution observation")
	}
	if s.pathRecorder(nil) != nil {
		t.Fatal("recorder without fan-out")
	}
	if gwei(new(big.Int).Lsh(big.NewInt(1), 200)) <= 0 || gwei(big.NewInt(999_999_999)) != 0 {
		t.Fatal("gwei conversion")
	}
}
