package exec

import (
	"encoding/json"
	"testing"
)

// A pending intent in the dispatch journal is validated on restart by
// re-encoding it against its ID. Decisions written by the window-count model
// must re-encode byte for byte.
func TestLegacyBidDecisionReencodesUnchanged(t *testing.T) {
	for _, legacy := range []string{
		`{"ModelTip":56378,"Wins":69,"Samples":73,"Expected":1234,"Applied":true,"Route":true,"Gas":328000,"Cap":8093008,"Worth":12255993,"Similar":true,"Rival":"0x2905b57c00000000000000000000000000000000","RivalN":12,"RivalBid":74877}`,
		`{"ModelTip":0,"Wins":0,"Samples":0,"Expected":-5,"Applied":false}`,
	} {
		var decision BidDecision
		if err := json.Unmarshal([]byte(legacy), &decision); err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(decision)
		if err != nil || string(again) != legacy {
			t.Fatalf("re-encoded\n%s\nas\n%s", legacy, again)
		}
	}
}
