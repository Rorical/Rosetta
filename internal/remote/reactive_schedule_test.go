package remote

import "testing"

// Reactive admission skips catch-up, and a context read at a replayed block
// can predate the configured executor. Catch-up must not refresh the reactive
// context every replayed block, which costs tens of thousands of failing
// calls per hour.
func TestReactiveContextRefreshWaitsForTheTip(t *testing.T) {
	cases := []struct {
		name                string
		catchingUp, nearTip bool
		want                bool
	}{
		{"live at the tip", false, true, true},
		{"live, tip unknown", false, false, true},
		{"replaying far behind", true, false, false},
		{"catch-up reached the tip", true, true, true},
	}
	for _, c := range cases {
		r := &runtime{catchingUp: c.catchingUp, nearTip: c.nearTip}
		if got := r.reactiveRefreshDue(); got != c.want {
			t.Errorf("%s: refresh due %v, want %v", c.name, got, c.want)
		}
	}
}
