package remote

import (
	"testing"
	"time"
)

func TestSilentStateFlagsStoppedBlocksAndAdmissions(t *testing.T) {
	now := time.Unix(10_000, 0)
	limit := time.Minute
	cases := []struct {
		name                   string
		canonicalAge, admitAge time.Duration
		reactive               bool
		want                   int64
	}{
		{"producing", time.Second, time.Second, true, 0},
		{"blocks stopped", 2 * time.Minute, time.Second, true, silentCanonical},
		{"admissions stopped", time.Second, 20 * time.Minute, true, silentAdmission},
		{"both", 2 * time.Minute, 20 * time.Minute, true, silentCanonical | silentAdmission},
		{"no reactive path", time.Second, 20 * time.Minute, false, 0},
	}
	for _, c := range cases {
		if got := silentState(now, now.Add(-c.canonicalAge), now.Add(-c.admitAge), limit, c.reactive); got != c.want {
			t.Errorf("%s: state %d, want %d", c.name, got, c.want)
		}
	}
}

// A canonical block that lands before the first check must not leave the
// admission clock unset, or every start reports admissions silent within
// seconds of starting.
func TestFirstCheckStartsTheAdmissionClock(t *testing.T) {
	now := time.Unix(10_000, 0)
	r := &runtime{reactive: &reactiveRuntime{}}
	r.silence.canonicalAt = now.Add(-10 * time.Millisecond)
	r.checkSilence(now)
	if r.silence.state != 0 {
		t.Fatalf("silent at start: state %d", r.silence.state)
	}
}
