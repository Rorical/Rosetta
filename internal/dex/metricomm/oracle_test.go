package metricomm

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func loadJSON[T any](t *testing.T, name string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return value
}

type fixtureFeed struct {
	Price        string `json:"price"`
	BidSpreadPpm uint32 `json:"bid_spread_ppm"`
	AskSpreadPpm uint32 `json:"ask_spread_ppm"`
	TimestampMS  uint64 `json:"timestamp_ms"`
}

func (f fixtureFeed) update(t *testing.T) FeedUpdate {
	t.Helper()
	price := mustBig(t, f.Price)
	if !price.IsUint64() {
		t.Fatalf("price %s outside uint64", price)
	}
	return FeedUpdate{Price1e8: price.Uint64(), BidSpreadPpm: f.BidSpreadPpm, AskSpreadPpm: f.AskSpreadPpm}
}

// TestDecodeOracleUpdateMatchesChain decodes real update transactions and
// requires what the oracle's getter returned for each feed afterwards.
func TestDecodeOracleUpdateMatchesChain(t *testing.T) {
	fixture := loadJSON[struct {
		Updates []struct {
			BlockNumber uint64        `json:"block_number"`
			Input       string        `json:"input"`
			TimestampMS uint64        `json:"timestamp_ms"`
			Feeds       []fixtureFeed `json:"feeds"`
		} `json:"updates"`
	}](t, "oracle-updates.json")
	if len(fixture.Updates) < 30 {
		t.Fatalf("%d updates in the fixture", len(fixture.Updates))
	}
	for _, row := range fixture.Updates {
		update, err := DecodeOracleUpdate(mustBytes(t, row.Input))
		if err != nil {
			t.Fatalf("block %d: %v", row.BlockNumber, err)
		}
		if update.TimestampMS != row.TimestampMS {
			t.Errorf("block %d: timestamp %d, chain %d", row.BlockNumber, update.TimestampMS, row.TimestampMS)
		}
		if len(row.Feeds) != OracleFeedCount {
			t.Fatalf("block %d: %d feeds", row.BlockNumber, len(row.Feeds))
		}
		for i, feed := range row.Feeds {
			if want := feed.update(t); update.Feeds[i] != want {
				t.Errorf("block %d feed %d: decoded %+v, chain %+v", row.BlockNumber, i, update.Feeds[i], want)
			}
		}
	}
}

// TestOracleCodesMatchDeployedOracle checks every spread code and the price
// word against the deployed oracle's own decoding.
func TestOracleCodesMatchDeployedOracle(t *testing.T) {
	fixture := loadJSON[struct {
		SpreadPpmByCode []uint32 `json:"spread_ppm_by_code"`
		PriceWords      []struct {
			Word  string `json:"word"`
			Price string `json:"price"`
		} `json:"price_words"`
	}](t, "oracle-codes.json")
	if len(fixture.SpreadPpmByCode) != 256 || len(fixture.PriceWords) == 0 {
		t.Fatalf("%d spread codes, %d price words", len(fixture.SpreadPpmByCode), len(fixture.PriceWords))
	}
	for code, want := range fixture.SpreadPpmByCode {
		input := make([]byte, oracleUpdateLength)
		input[5], input[12] = byte(code), byte(code)
		update, err := DecodeOracleUpdate(input)
		if err != nil {
			t.Fatal(err)
		}
		if update.Feeds[0].BidSpreadPpm != want || update.Feeds[1].AskSpreadPpm != want {
			t.Errorf("code %#x: %d and %d ppm, chain %d", code, update.Feeds[0].BidSpreadPpm, update.Feeds[1].AskSpreadPpm, want)
		}
	}
	for _, row := range fixture.PriceWords {
		input := make([]byte, oracleUpdateLength)
		copy(input[13:17], mustBytes(t, row.Word))
		update, err := DecodeOracleUpdate(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := new(big.Int).SetUint64(update.Feeds[2].Price1e8); got.Cmp(mustBig(t, row.Price)) != 0 {
			t.Errorf("price word %s: %s, chain %s", row.Word, got, row.Price)
		}
	}
}

func TestDecodeOracleUpdateRejectsOtherCalldata(t *testing.T) {
	valid := mustBytes(t, "0x0005f7046c03035e0e9a89525286c42d214b4b0000000000000001a1cbfd399900")
	if _, err := DecodeOracleUpdate(valid); err != nil {
		t.Fatal(err)
	}
	with := func(index int, value byte) []byte {
		changed := append([]byte(nil), valid...)
		changed[index] = value
		return changed
	}
	tests := []struct {
		name  string
		input []byte
	}{
		{"empty", nil},
		{"one byte short", valid[:32]},
		{"one byte long", append(append([]byte(nil), valid...), 0)},
		{"non-zero first byte", with(0, 1)},
		{"non-zero last byte", with(32, 1)},
		{"non-zero byte after the feeds", with(19, 1)},
		{"non-zero byte before the timestamp", with(23, 0x80)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeOracleUpdate(test.input); !errors.Is(err, ErrInvalidOracleUpdate) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

type quoteSample struct {
	BlockNumber        uint64        `json:"block_number"`
	BlockTimestamp     uint64        `json:"block_timestamp"`
	Pool               string        `json:"pool"`
	FeedIndex          int           `json:"feed_index"`
	OracleWord         string        `json:"oracle_word"`
	ExtensionStateWord string        `json:"extension_state_word"`
	Slot0              string        `json:"slot0"`
	Feeds              []fixtureFeed `json:"feeds"`
	BidX64             string        `json:"bid_x64"`
	AskX64             string        `json:"ask_x64"`
	ReferenceX64       string        `json:"reference_x64"`
	ExtensionBidX64    string        `json:"extension_bid_x64"`
	ExtensionAskX64    string        `json:"extension_ask_x64"`
}

type quoteFixture struct {
	QuoteExtension map[string]struct {
		ConfigWord     string `json:"config_word"`
		ParametersWord string `json:"parameters_word"`
	} `json:"quote_extension"`
	Samples []quoteSample `json:"samples"`
}

// TestQuoteMatchesChain rebuilds the quote the pools would have swapped on
// from raw chain data and compares it with traces of the real contracts. The
// oracle's storage word is decoded as an update, SkewPpm adds the quote
// extension's widening, and Quote must then give the extension's bid and ask
// to the last bit in every sample. The price provider's final quote may be
// wider still: it moves a side past its own reference when the reference
// leaves the quote, which is not modelled.
func TestQuoteMatchesChain(t *testing.T) {
	fixture := loadJSON[quoteFixture](t, "quotes.json")
	type tally struct {
		samples, plain, skewed, providerWider int
		maxSkewPpm                            uint32
		maxProviderPpm                        float64
	}
	byPool := map[string]*tally{}
	for _, sample := range fixture.Samples {
		update, err := DecodeOracleUpdate(append([]byte{0}, mustBytes(t, sample.OracleWord)...))
		if err != nil {
			t.Fatalf("block %d: %v", sample.BlockNumber, err)
		}
		for i, feed := range sample.Feeds {
			if want := feed.update(t); update.Feeds[i] != want || update.TimestampMS != feed.TimestampMS {
				t.Fatalf("block %d feed %d: decoded %+v at %d, chain %+v at %d",
					sample.BlockNumber, i, update.Feeds[i], update.TimestampMS, want, feed.TimestampMS)
			}
		}
		result := byPool[sample.Pool]
		if result == nil {
			result = &tally{}
			byPool[sample.Pool] = result
		}
		result.samples++

		words := fixture.QuoteExtension[sample.Pool]
		config := DecodeSkewConfig(mustHash(t, words.ConfigWord), mustHash(t, words.ParametersWord))
		bidSkew, askSkew := SkewPpm(mustHash(t, sample.Slot0), mustHash(t, sample.ExtensionStateWord), config, sample.BlockTimestamp)
		if bidSkew != 0 && askSkew != 0 {
			t.Fatalf("block %d: both sides widened (%d, %d)", sample.BlockNumber, bidSkew, askSkew)
		}
		feed := update.Feeds[sample.FeedIndex]
		bid, ask := Quote(feed.Price1e8, feed.BidSpreadPpm+bidSkew, feed.AskSpreadPpm+askSkew)
		if bid == nil || ask == nil {
			t.Fatalf("block %d: no quote from %+v", sample.BlockNumber, feed)
		}
		if bid.String() != sample.ExtensionBidX64 || ask.String() != sample.ExtensionAskX64 {
			t.Errorf("block %d %s: quote [%s, %s] with skew (%d, %d) ppm, extension returned [%s, %s]",
				sample.BlockNumber, sample.Pool, bid, ask, bidSkew, askSkew, sample.ExtensionBidX64, sample.ExtensionAskX64)
			continue
		}
		if bidSkew+askSkew == 0 {
			result.plain++
		} else {
			result.skewed++
			result.maxSkewPpm = max(result.maxSkewPpm, bidSkew+askSkew)
		}

		chainBid, chainAsk, reference := mustBig(t, sample.BidX64), mustBig(t, sample.AskX64), mustBig(t, sample.ReferenceX64)
		if chainBid.Cmp(bid) == 0 && chainAsk.Cmp(ask) == 0 {
			continue
		}
		// The provider only widens, and only the side its reference crossed.
		result.providerWider++
		if chainBid.Cmp(bid) > 0 || chainAsk.Cmp(ask) < 0 || (reference.Cmp(bid) >= 0 && reference.Cmp(ask) <= 0) {
			t.Errorf("block %d %s: provider quote [%s, %s] around reference %s, extension [%s, %s]",
				sample.BlockNumber, sample.Pool, chainBid, chainAsk, reference, bid, ask)
		}
		for _, side := range [][2]*big.Int{{bid, chainBid}, {chainAsk, ask}} {
			widening := new(big.Float).SetInt(new(big.Int).Sub(side[0], side[1]))
			ppm, _ := widening.Quo(widening, new(big.Float).SetInt(bid)).Float64()
			result.maxProviderPpm = max(result.maxProviderPpm, ppm*1e6)
		}
	}
	if len(byPool) != 2 {
		t.Fatalf("%d pools in the fixture", len(byPool))
	}
	for pool, result := range byPool {
		t.Logf("%s: %d samples bit-exact against the extension (%d without skew, %d with skew up to %d ppm); provider wider in %d (up to %.1f ppm)",
			pool, result.samples, result.plain, result.skewed, result.maxSkewPpm, result.providerWider, result.maxProviderPpm)
		if result.plain < 20 || result.skewed == 0 {
			t.Errorf("%s: %d samples without skew, %d with", pool, result.plain, result.skewed)
		}
	}
}

func TestSkewPpm(t *testing.T) {
	// 200 ppm bins, 40% gain, ten minutes to relax.
	config := DecodeSkewConfig(
		mustHash(t, "0x0000000000010101000a00c8e844701934d9c6085b292e428f41b911cc0b3872"),
		mustHash(t, "0x000000000000000000000000000000000000000000000258000100000fa00fa0"),
	)
	if config != (SkewConfig{BinLengthPpm: 200, GainE4: 4000, RelaxSeconds: 600}) {
		t.Fatalf("config %+v", config)
	}
	const updatedAt = 1_794_417_056
	q32 := func(bins float64) uint64 { return uint64(int64(bins * (1 << 32))) }
	state := func(level, target float64) domain.Hash {
		var word domain.Hash
		binary.BigEndian.PutUint32(word[12:16], updatedAt)
		binary.BigEndian.PutUint64(word[16:24], q32(target))
		binary.BigEndian.PutUint64(word[24:32], q32(level))
		return word
	}
	// Slot 0 with the pool half way through the given bin.
	slot0 := func(binIdx int16) domain.Hash {
		var word domain.Hash
		binary.BigEndian.PutUint16(word[29:31], uint16(binIdx))
		word[16] = 0x80
		return word
	}
	tests := []struct {
		name          string
		binIdx        int16
		level, target float64
		elapsed       uint64
		bid, ask      uint32
	}{
		{"within one bin of the level", -6, -6, -6, 0, 0, 0},
		{"exactly one bin above", -5, -5.5, -5.5, 0, 0, 0},
		{"two bins above widens the bid", -4, -5.5, -5.5, 0, 80, 0},
		{"two bins below widens the ask", -8, -5.5, -5.5, 0, 0, 80},
		{"a fraction rounds up", -4, -5.25, -5.25, 0, 60, 0},
		{"level half way to its target", -4, -6.5, -4.5, 300, 80, 0},
		{"level at its target", -4, -6.5, -4.5, 600, 0, 0},
		{"level stays at its target", -4, -6.5, -4.5, 100_000, 0, 0},
		{"no time elapsed", -4, -6.5, -4.5, 0, 160, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bid, ask := SkewPpm(slot0(test.binIdx), state(test.level, test.target), config, updatedAt+test.elapsed)
			if bid != test.bid || ask != test.ask {
				t.Fatalf("skew (%d, %d), want (%d, %d)", bid, ask, test.bid, test.ask)
			}
		})
	}
	// A block timestamp before the last update counts as no time elapsed.
	if bid, ask := SkewPpm(slot0(-4), state(-6.5, -4.5), config, updatedAt-50); bid != 160 || ask != 0 {
		t.Fatalf("before the update: skew (%d, %d)", bid, ask)
	}
}

func TestQuoteWithoutPositiveBid(t *testing.T) {
	for _, test := range []struct {
		name      string
		price     uint64
		bidSpread uint32
	}{
		{"no price", 0, 100},
		{"bid spread of the whole price", 1_000_000, 1_000_000},
		{"bid spread above the price", 1_000_000, 2_000_000},
	} {
		if bid, ask := Quote(test.price, test.bidSpread, 100); bid != nil || ask != nil {
			t.Errorf("%s: got [%s, %s]", test.name, bid, ask)
		}
	}
	// The smallest price still quotes: bid floor(2^64 * 999900 / 1e14), ask one above the floor.
	bid, ask := Quote(1, 100, 100)
	if bid.String() != "184448993993" || ask.String() != "184485887482" {
		t.Errorf("got [%s, %s]", bid, ask)
	}
}
