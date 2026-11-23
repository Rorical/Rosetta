package remote

import (
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
)

func TestFlashblockVersionBindsEmptyInitialFrame(t *testing.T) {
	store, pools := poolFixture(t)
	versions := make([]domain.StateVersion, 2)
	for i := range versions {
		branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
		if err != nil {
			t.Fatal(err)
		}
		r := &runtime{store: store, fast: branch}
		// The first frame may contain only execution context. Later identical
		// transactions must remain bound to that initial context.
		first := feed.Flashblock{PayloadID: "0x0102030405060708", Number: 11, Base: &feed.FlashblockBase{
			Number: 11, Parent: store.Block().Hash, Timestamp: store.Block().Timestamp + uint64(i) + 1,
		}}
		heartbeat := feed.Flashblock{PayloadID: first.PayloadID, Number: 11, Index: 1}
		transaction := feed.Flashblock{PayloadID: first.PayloadID, Number: 11, Index: 2, Transactions: []feed.FlashTransaction{
			{Hash: domain.Hash{1}, Raw: []byte{1}, Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3_000_000)}},
		}}
		for _, frame := range []feed.Flashblock{first, heartbeat, transaction} {
			if err := r.acceptFlashblock(t.Context(), frame, time.Time{}); err != nil {
				t.Fatal(err)
			}
		}
		versions[i] = r.tentative
	}
	if versions[0].PrefixHash == versions[1].PrefixHash {
		t.Fatal("prefix version lost execution context from empty initial frames")
	}
}
