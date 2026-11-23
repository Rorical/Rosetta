package ingest

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/state"
)

func TestFastResetToRejectsOldPrefixEvenWhenNewBaselineIsInvalid(t *testing.T) {
	for _, baseline := range []*state.Store{nil, state.New()} {
		fast, standard, _, key := fastBranch(t, FastOptions{})
		if _, err := fast.Apply(t.Context(), fastSync(key, 11, 0, 1, 300)); err != nil {
			t.Fatal(err)
		}
		_, old, _, _ := fast.View()
		if err := fast.ResetTo(baseline); err == nil {
			t.Fatal("invalid recovery baseline was accepted")
		}
		if fast.Current(old) {
			t.Fatal("failed baseline replacement left the old Fast prefix usable")
		}
		if err := fast.ResetTo(standard.Base()); err != nil {
			t.Fatal(err)
		}
		if _, err := fast.Apply(t.Context(), fastSync(key, 11, 0, 2, 500)); err != nil {
			t.Fatal(err)
		}
		_, current, _, _ := fast.View()
		if current.Epoch <= old.Epoch {
			t.Fatal("recovery reused the old Fast epoch")
		}
	}
}
