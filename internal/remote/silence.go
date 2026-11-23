package remote

import (
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/telemetry"
)

// A running pipeline can stop producing without failing: a rate-limited or
// stuck follower stops canonical blocks, or blocks keep coming but the
// reactive path admits nothing. Without a check a lane can stay dark for
// hours that way.
const (
	silenceCanonicalBlocks = 30
	silenceCanonicalFloor  = time.Minute
	silenceAdmission       = 15 * time.Minute
	silenceReportEvery     = time.Hour
)

// Bits of the pipeline_silent gauge.
const (
	silentCanonical int64 = 1 << iota
	silentAdmission
)

type silence struct {
	canonicalAt, admittedAt time.Time
	state                   int64
	reportedAt              time.Time
}

// silentState returns the pipeline_silent bits at now.
func silentState(now, canonicalAt, admittedAt time.Time, canonicalLimit time.Duration, reactive bool) int64 {
	var state int64
	if now.Sub(canonicalAt) > canonicalLimit {
		state |= silentCanonical
	}
	if reactive && now.Sub(admittedAt) > silenceAdmission {
		state |= silentAdmission
	}
	return state
}

// checkSilence runs on the main loop's timer.
func (r *runtime) checkSilence(now time.Time) {
	// The clocks start independently: the first canonical block usually lands
	// before the first check.
	if r.silence.canonicalAt.IsZero() {
		r.silence.canonicalAt = now
	}
	if r.silence.admittedAt.IsZero() {
		r.silence.admittedAt = now
	}
	limit := max(silenceCanonicalFloor, time.Duration(silenceCanonicalBlocks*r.cfg.Network.BlockTimeMS)*time.Millisecond)
	state := silentState(now, r.silence.canonicalAt, r.silence.admittedAt, limit, r.reactive != nil)
	r.metrics.Set(telemetry.Standard, telemetry.PipelineSilent, state)
	switch {
	case state != 0 && (state != r.silence.state || now.Sub(r.silence.reportedAt) >= silenceReportEvery):
		slog.Error("pipeline silent", "canonical_blocks_stopped", state&silentCanonical != 0, "reactive_admissions_stopped", state&silentAdmission != 0,
			"last_canonical", r.silence.canonicalAt.UTC(), "last_admission", r.silence.admittedAt.UTC())
		r.silence.reportedAt = now
	case state == 0 && r.silence.state != 0:
		slog.Info("pipeline producing again")
	}
	r.silence.state = state
}
