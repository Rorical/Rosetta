package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
	"github.com/gofrs/flock"
)

func TestReactiveFeedObservationPersistsAndInvalidates(t *testing.T) {
	r, event, _ := liveReactiveFixture(t)
	s := r.reactive
	s.observations = make(chan any, 4)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(s.primary())
	if job == nil {
		t.Fatal("no intent")
	}
	result := s.executeJob(t.Context(), job, s.primary())
	hash, ok := result["transaction"].(domain.Hash)
	if !ok || hash == (domain.Hash{}) {
		t.Fatal(result)
	}
	pos := execution.ExecutionPosition{Stream: "flashblocks", PayloadID: "payload", BlockNumber: 10, FrameIndex: 2, TransactionIndex: 4, ReceivedAtNS: time.Now().UnixNano()}
	r.observeReactiveExecution(hash, false, nil, pos)
	observation := s.lastObserved
	if observation == nil || observation.Success || observation.Submission == nil || !s.dispatcher.Included() {
		t.Fatal(observation)
	}
	pos.ReceivedAtNS++
	r.observeReactiveExecution(hash, false, nil, pos)
	if len(s.observations) != 1 {
		t.Fatal("duplicate observation")
	}
	r.invalidateReactiveObservation("stream_reset", 0)
	if !s.dispatcher.Busy() || len(s.observations) != 2 {
		t.Fatal("reset released nonce or lost invalidation")
	}

	path := filepath.Join(t.TempDir(), "observations.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	if err = lock.Lock(); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual asynchronous writer's shutdown drain and file sync.
	writer := &reactiveRuntime{observations: s.observations, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer.run(ctx, file, lock)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var value map[string]any
		if err = json.Unmarshal(line, &value); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, value)
	}
	if len(lines) != 2 || lines[0]["type"] != "execution_observed" || lines[1]["type"] != "execution_observation_invalidated" {
		t.Fatal(string(raw))
	}
}

// The writer books our feed sighting as it persists it, before any receipt;
// paid fees arriving later feed the bid model's gas ratio.
func TestReactiveWriterSettlesFeedSighting(t *testing.T) {
	r, event, _ := liveReactiveFixture(t)
	s := r.reactive
	s.observations = make(chan any, 4)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(s.primary())
	if job == nil {
		t.Fatal("no intent")
	}
	hash, ok := s.executeJob(t.Context(), job, s.primary())["transaction"].(domain.Hash)
	if !ok || hash == (domain.Hash{}) {
		t.Fatal("not sent")
	}
	r.observeReactiveExecution(hash, false, nil, execution.ExecutionPosition{Stream: "flashblocks", BlockNumber: 10, ReceivedAtNS: time.Now().UnixNano()})

	path := filepath.Join(t.TempDir(), "reactive.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	if err = lock.Lock(); err != nil {
		t.Fatal(err)
	}
	writer := &reactiveRuntime{observations: s.observations, done: make(chan struct{}), dispatcher: s.dispatcher, metrics: &telemetry.Registry{}, bids: newBidEstimator(time.Second, 2*time.Second)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer.run(ctx, file, lock)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	type outputRecord struct {
		Type    string                `json:"type"`
		Receipt *execution.Settlement `json:"receipt"`
	}
	var records []outputRecord
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var record outputRecord
		if err = json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 2 || records[0].Type != "execution_observed" || records[1].Type != "settlement" {
		t.Fatal(string(raw))
	}
	if settled := records[1].Receipt; settled.Hash != hash || settled.Success || settled.FeeStatus != execution.FeesPending || settled.Fees != nil {
		t.Fatalf("feed settlement %+v", settled)
	}
	if s.dispatcher.Busy() {
		t.Fatal("feed settlement kept the lane busy")
	}

	paid := &execution.Settlement{Hash: hash, Success: true, Intent: &job.intent, Fees: &domain.ReceiptFees{GasUsed: job.intent.Call.GasLimit / 2, Total: big.NewInt(1)}}
	write := func(any) bool { return true }
	if !writer.publishSettlements([]execution.SettlementUpdate{{Settlement: paid, FeesBooked: true}}, nil, write) || len(writer.bids.gasRatios) != 1 || writer.bids.gasRatios[0] != 0.5 {
		t.Fatal("paid gas did not reach the bid model", writer.bids.gasRatios)
	}
}

func TestObservationOverflowClosesSendGuard(t *testing.T) {
	r, event, network := liveReactiveFixture(t)
	s := r.reactive
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(s.primary())
	s.observations = make(chan any, 1)
	s.enqueueObservation("first")
	s.enqueueObservation("overflow")
	if s.failure.Load() == nil {
		t.Fatal("overflow ignored")
	}
	s.executeJob(t.Context(), job, s.primary())
	if network.calls != 0 {
		t.Fatal("broadcast after loss of observation persistence")
	}
}

// A send the feed never showed settles from the canonical block the follower
// already read, fees included, without a receipt read.
func TestCanonicalBlockSettlesSendTheFeedMissed(t *testing.T) {
	r, event, _ := liveReactiveFixture(t)
	s := r.reactive
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	job := s.takeJob(s.primary())
	if job == nil {
		t.Fatal("no intent")
	}
	hash, ok := s.executeJob(t.Context(), job, s.primary())["transaction"].(domain.Hash)
	if !ok || hash == (domain.Hash{}) {
		t.Fatal("not sent")
	}
	other := ingest.Transaction{Hash: domain.Hash{0xee}, Successful: true}
	ours := ingest.Transaction{Hash: hash, Fees: &domain.ReceiptFees{GasUsed: 21000, Total: big.NewInt(7)}}
	r.observeReactiveCanonical(canonicalUpdate{block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}}, transactions: []ingest.Transaction{other, ours}})
	updates, err := s.dispatcher.Watch(time.Now())
	if err != nil || len(updates) != 1 || !updates[0].First || !updates[0].FeesBooked || updates[0].Settlement.Hash != hash || updates[0].Settlement.Block.Number != 10 {
		t.Fatal("canonical sighting not settled", updates, err)
	}
	if s.dispatcher.Busy() || s.dispatcher.Status(time.Now()).Included != 0 {
		t.Fatal("canonical settlement left the nonce open")
	}
}
