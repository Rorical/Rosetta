package ingest

import (
	"testing"
)

func TestFastDependencyRefreshRetainsPrefixAndRetiresOldVersion(t *testing.T) {
	fast, standard, _, key := fastBranch(t, FastOptions{})
	ref := standard.Base().Block()
	first := fastSync(key, ref.Number+1, 0, 1, 300)
	if _, err := fast.Apply(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	_, old, _, _ := fast.View()
	if err := fast.RefreshBase(t.Context(), standard.Base()); err != nil {
		t.Fatal(err)
	}
	_, next, _, ok := fast.View()
	if !ok || next.Anchor != old.Anchor || next.TxIndex != old.TxIndex || next.Epoch == old.Epoch || fast.Current(old) {
		t.Fatal("dependency refresh lost or reused its prefix version", old, next)
	}
	second := fastSync(key, ref.Number+1, 1, 2, 500)
	if _, err := fast.Apply(t.Context(), second); err != nil {
		t.Fatal("next stream position rejected", err)
	}
}
