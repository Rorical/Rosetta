package metricomm

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type referenceQuote struct {
	Base   uint64 `json:"base"`
	Quote  uint64 `json:"quote"`
	ExtBid string `json:"ext_bid"`
	ExtAsk string `json:"ext_ask"`
	Bid    string `json:"bid"`
	Ask    string `json:"ask"`
}

// The provider's quote in 15 swaps through an Arbitrum One WETH/USDC pool:
// the extension's quote and the two reference feeds it read, and the
// quote it returned. In 11 of them one side was moved to the reference band.
// The modelled quote is never better for the taker than the chain's and is
// within 2^-59 of it.
func TestQuoteIsHeldToTheReferenceBand(t *testing.T) {
	clamped := 0
	for i, c := range loadJSON[[]referenceQuote](t, "reference-quotes.json") {
		number := func(text string) *big.Int {
			value, ok := new(big.Int).SetString(text, 0)
			if !ok {
				t.Fatalf("case %d: %q", i, text)
			}
			return value
		}
		metric := &pool.MetricState{Reference: domain.Address{1}, ReferenceBase1e8: c.Base, ReferenceQuote1e8: c.Quote, BidX64: number(c.ExtBid), AskX64: number(c.ExtAsk)}
		clampToReference(metric)
		bid, ask := number(c.Bid), number(c.Ask)
		if bid.Cmp(number(c.ExtBid)) != 0 || ask.Cmp(number(c.ExtAsk)) != 0 {
			clamped++
		}
		slack := new(big.Int).Rsh(ask, 59)
		if metric.BidX64.Cmp(bid) > 0 || new(big.Int).Sub(bid, metric.BidX64).Cmp(slack) > 0 {
			t.Errorf("case %d: bid %s, chain %s", i, metric.BidX64, bid)
		}
		if metric.AskX64.Cmp(ask) < 0 || new(big.Int).Sub(metric.AskX64, ask).Cmp(slack) > 0 {
			t.Errorf("case %d: ask %s, chain %s", i, metric.AskX64, ask)
		}
	}
	if clamped != 11 {
		t.Fatalf("%d cases moved by the band, want 11", clamped)
	}
}

type referenceUpdateFixture struct {
	Input string `json:"input"`
	Words []struct {
		Feed uint32 `json:"feed"`
		Slot string `json:"slot"`
		Word string `json:"word"`
	} `json:"words"`
}

// One update transaction of the reference oracle and the four words it
// stored (Arbitrum One, prestate trace): the decoded prices and
// timestamp are the stored ones, at the slots ReferenceSlot derives.
func TestReferenceUpdateDecodesToTheStoredWords(t *testing.T) {
	fixture := loadJSON[referenceUpdateFixture](t, "reference-update.json")
	input, err := hex.DecodeString(strings.TrimPrefix(fixture.Input, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	update, err := DecodeReferenceUpdate(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(update.Feeds) != len(fixture.Words) {
		t.Fatalf("%d feeds, want %d", len(update.Feeds), len(fixture.Words))
	}
	for i, want := range fixture.Words {
		price, timestamp := DecodeReferenceWord(mustHash(t, want.Word))
		if feed := update.Feeds[i]; feed.ID != want.Feed || feed.Price1e8 != price || update.TimestampMS != timestamp {
			t.Errorf("feed %d: decoded %+v at %d, stored %d at %d", want.Feed, feed, update.TimestampMS, price, timestamp)
		}
		if slot := ReferenceSlot(want.Feed); slot != mustHash(t, want.Slot) {
			t.Errorf("feed %d: slot %s", want.Feed, slot.Hex())
		}
	}
	for _, cut := range []int{1, 20, 80, len(input) - 1} {
		if _, err := DecodeReferenceUpdate(input[:cut]); err == nil {
			t.Errorf("accepted an update cut to %d bytes", cut)
		}
	}
	exponent := append([]byte(nil), input...)
	exponent[2+16+4+65+2+14+5+9+2] ^= 1
	if _, err := DecodeReferenceUpdate(exponent); err == nil {
		t.Error("accepted another price exponent")
	}
}

// A pool registered with a reference has no quote until both feeds are
// known, takes each feed's newer update only, and loses the quote again when
// an update cannot be read.
func TestReferenceFeedsGateTheQuote(t *testing.T) {
	metric := &pool.MetricState{Reference: domain.Address{1}, ReferenceBaseFeed: 2, ReferenceQuoteFeed: 7,
		Words: []domain.Hash{{}}, Price1e8: 200_000_000_000, BidSpreadPpm: 100, AskSpreadPpm: 100}
	price(metric, 0)
	if metric.BidX64 != nil || metric.AskX64 != nil {
		t.Fatal("quoted without the reference feeds")
	}
	FollowReference(metric, ReferenceUpdate{TimestampMS: 10, Feeds: []ReferenceFeed{{ID: 7, Price1e8: 100_000_000}}}, 0)
	if metric.BidX64 != nil {
		t.Fatal("quoted with one reference feed")
	}
	// The reference sits 1% above the feed: the ask follows it.
	FollowReference(metric, ReferenceUpdate{TimestampMS: 20, Feeds: []ReferenceFeed{{ID: 2, Price1e8: 202_000_000_000}, {ID: 8, Price1e8: 1}}}, 0)
	_, unclamped := Quote(metric.Price1e8, 100, 100)
	if metric.AskX64 == nil || metric.AskX64.Cmp(unclamped) <= 0 {
		t.Fatalf("ask %v not above the extension's %s", metric.AskX64, unclamped)
	}
	held := new(big.Int).Set(metric.AskX64)
	FollowReference(metric, ReferenceUpdate{TimestampMS: 15, Feeds: []ReferenceFeed{{ID: 2, Price1e8: 300_000_000_000}}}, 0)
	if metric.AskX64.Cmp(held) != 0 {
		t.Fatal("an older update replaced the feed")
	}
	ForgetReference(metric)
	if metric.BidX64 != nil || metric.ReferenceBase1e8 != 0 {
		t.Fatal("the quote outlived the reference")
	}
	// Without a registered reference the quote is the extension's.
	free := &pool.MetricState{Words: []domain.Hash{{}}, Price1e8: 200_000_000_000}
	price(free, 0)
	if free.BidX64 == nil {
		t.Fatal("a pool without a reference lost its quote")
	}
}
