package exec

import (
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDispatchStatusReportsLaneBudgetAndStop(t *testing.T) {
	i, l, s := dispatchFixture(t)
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "dispatch.json"), l, s, &dispatchNetworkFixture{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	idle := d.Status(now)
	if idle.Pending != nil || idle.Stopped || idle.HaltedReason != "" || idle.SpentToday.Sign() != 0 || idle.DailyCap.Cmp(l.PerDay) != 0 {
		t.Fatalf("idle status: %+v", idle)
	}
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	sent := d.Status(now)
	if sent.Pending == nil || *sent.Pending != hash || sent.Variants != 1 || sent.NextNonce != 0 || sent.SpentToday.Cmp(i.Budget) != 0 {
		t.Fatalf("pending status: %+v", sent)
	}
	// The budget opens lazily on a new day; only the unsettled send carries over.
	d.store.state.Charged = new(big.Int).Add(i.Budget, big.NewInt(7))
	if next := d.Status(now.Add(24 * time.Hour)); next.SpentToday.Cmp(i.Budget) != 0 {
		t.Fatalf("next day spent: %s", next.SpentToday)
	}
	if err := os.WriteFile(l.StopFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.store.state.HaltedReason = "fee overrun"
	if halted := d.Status(now); !halted.Stopped || halted.HaltedReason != "fee overrun" {
		t.Fatalf("halt status: %+v", halted)
	}
}
