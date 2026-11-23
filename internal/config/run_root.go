package config

import "fmt"

// ValidateRunRoot rejects combinations that would leave a rotated output with
// two owners. Under -run-root the instance names every per-run file itself, and
// the rotation request and the console both arrive through the control socket.
func (c Config) ValidateRunRoot() error {
	if c.RunRoot == "" {
		return nil
	}
	for _, explicit := range []struct{ flag, value string }{
		{"-candidate-out", c.CandidateOut}, {"-research-out", c.ResearchOut}, {"-replay-out", c.ReplayOut},
		{"-pool-snapshot-out", c.PoolSnapshotOut}, {"-metrics-out", c.MetricsOut},
	} {
		if explicit.value != "" {
			return fmt.Errorf("-run-root names its own run outputs; remove %s", explicit.flag)
		}
	}
	if c.ControlDir == "" {
		return fmt.Errorf("-run-root requires -control-dir")
	}
	if c.SearchConfig == "" {
		return fmt.Errorf("-run-root requires -search-config")
	}
	return nil
}
