package bidreport

import (
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"math/big"
	"slices"
	"testing"
	"time"
)

func TestFailureEvidenceSeparatesTimingBidAndExecutionAndKeepsUnknown(t *testing.T) {
	at := time.Unix(1800000000, 0)
	pool := domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{1}, PoolID: domain.Hash{1}}
	other := pool
	other.PoolID = domain.Hash{2}
	intent := &execution.ReactiveIntent{Version: domain.StateVersion{BlockNumber: 20, TxIndex: 2}}
	intent.Call.TipCap = big.NewInt(10)
	intent.Route.Steps = []execution.ExecutorStep{{Pool: pool}}
	p := prediction{intent: intent, at: at, receivedNS: at.Add(-40 * time.Millisecond).UnixNano()}
	o := &execution.Settlement{Position: &execution.ExecutionPosition{BlockNumber: 20, FrameIndex: 3, TransactionIndex: 6, ReceivedAtNS: at.Add(100 * time.Millisecond).UnixNano()}}
	early := Competition{Block: 20, Frame: 2, Index: 3, ReceivedNS: at.Add(-10 * time.Millisecond).UnixNano(), Pools: []domain.PoolKey{pool}, Tip: big.NewInt(1)}
	higher := Competition{Block: 20, Frame: 3, Index: 4, ReceivedNS: at.Add(100 * time.Millisecond).UnixNano(), Pools: []domain.PoolKey{pool}, Tip: big.NewInt(11)}
	flags := indexCompetition([]Competition{early, higher}).classify(p, o, "pool_moved:0")
	for _, want := range []string{"shared_pool_changed_before_broadcast", "higher_tip_same_frame_predecessor", "pool_price_guard_failed"} {
		if !slices.Contains(flags, want) {
			t.Fatal(flags)
		}
	}
	higher.Pools = []domain.PoolKey{other}
	early.Index = 2 // already included in the quote's state
	flags = indexCompetition([]Competition{early, higher}).classify(p, o, "")
	if !slices.Equal(flags, []string{"unexplained_revert"}) {
		t.Fatal("address-only collision or trigger state contamination", flags)
	}
	flags = evidenceIndex{}.classify(p, o, "transfer_shortfall_recorded")
	if !slices.Equal(flags, []string{"execution_transfer_shortfall"}) {
		t.Fatal(flags)
	}
}

func TestEconomicsIncludesChangedTipsAndCancellationFees(t *testing.T) {
	j := &journal{t: t}
	at := time.Unix(1800000000, 0)
	a := j.send(1, .5, at, "fast")
	a.Cancelled = true
	j.settle(a)
	b := j.send(2, .5, at, "fast")
	j.settle(b)
	r, err := Read(t.Context(), &j.Buffer, Options{Limit: 100})
	if err != nil || r.Economics.Paid != 2 || r.Economics.Cancelled != 1 || r.Economics.Reverted != 1 || r.Economics.Net.Int64() != -20 {
		t.Fatal(r, err)
	}
}
