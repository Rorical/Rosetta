package remote

import (
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Sends that wait seconds in the sequencer's queue land tens of blocks after
// their quote and revert, so a slow answer holds the sends behind it.
func TestSlowSequencerAnswerHoldsSends(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var b sequencerBacklog
	b.observe(400*time.Millisecond, now)
	if b.held(now) {
		t.Fatal("an ordinary answer time holds sends")
	}
	b.observe(backlogAck, now)
	if !b.held(now.Add(backlogHold - time.Millisecond)) {
		t.Fatal("a slow answer does not hold sends")
	}
	if b.held(now.Add(backlogHold)) {
		t.Fatal("the hold does not end")
	}
	// A quick answer during a hold does not end it early.
	b.observe(backlogAck, now)
	b.observe(50*time.Millisecond, now.Add(time.Second))
	if !b.held(now.Add(2 * time.Second)) {
		t.Fatal("a quick answer ended the hold")
	}
}

func TestBacklogHoldStopsANewSend(t *testing.T) {
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
	s.backlog.observe(backlogAck, time.Now())
	result := s.executeJob(t.Context(), job, s.primary())
	if result["status"] != "sequencer_backlog" {
		t.Fatalf("status %v, want sequencer_backlog", result["status"])
	}
	if hash, _ := result["transaction"].(domain.Hash); hash != (domain.Hash{}) {
		t.Fatal("sent during a backlog hold")
	}
}
