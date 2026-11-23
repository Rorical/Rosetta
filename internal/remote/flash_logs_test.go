package remote

import (
	"encoding/json"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"testing"
)

func TestFlashblocksReplayLogOrderAndRevertedChildren(t *testing.T) {
	raw := `{"gasUsed":"0x100","logs":[{"address":"0x0000000000000000000000000000000000000001","topics":[],"data":"0x01","position":"0x0"},{"address":"0x0000000000000000000000000000000000000001","topics":[],"data":"0x04","position":2}],"calls":[{"logs":[{"address":"0x0000000000000000000000000000000000000002","topics":[],"data":"0x02","position":0}]},{"error":"execution reverted","logs":[{"address":"0x0000000000000000000000000000000000000003","topics":[],"data":"0x03","position":0}]}]}`
	var frame flashCallFrame
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatal(err)
	}
	logs, err := frame.logs()
	if err != nil || len(logs) != 3 || logs[0].Data[0] != 1 || logs[1].Data[0] != 2 || logs[2].Data[0] != 4 {
		t.Fatal("incorrect interleaved receipt logs", logs, err)
	}
	frame.Logs[0].Position = nil
	if _, err := frame.logs(); err == nil {
		t.Fatal("ambiguous log order accepted")
	}
}

func TestFlashblocksPrefixBoundsIncludeReceipts(t *testing.T) {
	prefix := flashPrefix{bytes: 32 << 20}
	if err := prefix.append(feed.Flashblock{}, 1); err == nil {
		t.Fatal("receipt/frame bytes escaped prefix bound")
	}
	prefix = flashPrefix{}
	tx := feed.FlashTransaction{Hash: domain.Hash{1}, Raw: []byte{1}}
	if err := prefix.append(feed.Flashblock{Transactions: []feed.FlashTransaction{tx}}, 100); err != nil {
		t.Fatal(err)
	}
	if err := prefix.append(feed.Flashblock{Transactions: []feed.FlashTransaction{tx}}, 100); err == nil {
		t.Fatal("repeated transaction accepted")
	}
}
