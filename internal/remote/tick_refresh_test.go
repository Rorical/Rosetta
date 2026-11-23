package remote

import (
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
)

func TestTickRefreshContinuesSameFlashblocksPayload(t *testing.T) {
	store, pools := poolFixture(t)
	fast, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := &runtime{store: store, fast: fast}
	block := feed.Flashblock{Number: store.Block().Number + 1, PayloadID: "payload", Base: &feed.FlashblockBase{Number: store.Block().Number + 1, Parent: store.Block().Hash}, Transactions: []feed.FlashTransaction{{Hash: digest([]byte{1}), Raw: []byte{1}, Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3000000)}}}}
	if err = r.acceptFlashblock(t.Context(), block, time.Time{}); err != nil {
		t.Fatal(err)
	}
	old := r.tentative
	cursor := r.flash
	if err = r.refreshTentativeDependencies(t.Context(), []domain.PoolKey{pools[0].Key}); err != nil {
		t.Fatal(err)
	}
	if r.flash != cursor || r.tentative.Epoch == old.Epoch || r.tentative.PrefixHash != old.PrefixHash || r.Current(old) {
		t.Fatal("repair lost stream cursor or reused old ticket")
	}
	block.Base = nil
	block.Index = 1
	block.Transactions[0].Hash = digest([]byte{2})
	block.Transactions[0].Raw = []byte{2}
	if err = r.acceptFlashblock(t.Context(), block, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if r.tentative.TxIndex != 1 || r.flash.nextTransaction != 2 {
		t.Fatal("could not consume the next frame after dependency repair")
	}
}
