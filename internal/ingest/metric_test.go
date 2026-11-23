package ingest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

type metricQuoteFixture struct {
	Oracle         string `json:"oracle"`
	QuoteExtension map[string]struct {
		ConfigWord     string `json:"config_word"`
		ParametersWord string `json:"parameters_word"`
	} `json:"quote_extension"`
	Samples []struct {
		BlockTimestamp     uint64 `json:"block_timestamp"`
		Pool               string `json:"pool"`
		FeedIndex          uint8  `json:"feed_index"`
		OracleWord         string `json:"oracle_word"`
		ExtensionStateWord string `json:"extension_state_word"`
		Slot0              string `json:"slot0"`
		ExtensionBidX64    string `json:"extension_bid_x64"`
		ExtensionAskX64    string `json:"extension_ask_x64"`
	} `json:"samples"`
}

func metricHash(t *testing.T, s string) domain.Hash {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(raw) != 32 {
		t.Fatalf("hash %q: %v", s, err)
	}
	return domain.Hash(raw)
}

func rawCall(t *testing.T, to domain.Address, input []byte) []byte {
	t.Helper()
	target := common.Address(to)
	raw, err := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(42161), To: &target, Data: input, Gas: 400_000, GasFeeCap: big.NewInt(1), GasTipCap: big.NewInt(1)}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The oracle's event names the feed and carries no price; the price is in the
// update's calldata. Every sample of the chain's own quotes (Arbitrum One)
// must come out of the transaction alone, on both pools at once.
func TestOracleUpdateTransactionSetsTheQuoteOfItsPools(t *testing.T) {
	var fixture metricQuoteFixture
	raw, err := os.ReadFile("../dex/metricomm/testdata/quotes.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	oracle, err := domain.ParseAddress(fixture.Oracle)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.Samples) < 40 {
		t.Fatalf("%d samples", len(fixture.Samples))
	}
	for _, sample := range fixture.Samples {
		address, err := domain.ParseAddress(sample.Pool)
		if err != nil {
			t.Fatal(err)
		}
		words := fixture.QuoteExtension[sample.Pool]
		key := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: address}
		other := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: domain.Address{9}}
		store := state.New()
		for _, k := range []domain.PoolKey{key, other} {
			metric := &pool.MetricState{
				Token0Scale: big.NewInt(1), Token1Scale: big.NewInt(1), HighestBin: 0, LowestBin: 0,
				Words:  []domain.Hash{metricHash(t, sample.Slot0), {}, {}},
				Oracle: oracle, FeedIndex: sample.FeedIndex,
				SkewConfig: metricHash(t, words.ConfigWord), SkewParameters: metricHash(t, words.ParametersWord), SkewState: metricHash(t, sample.ExtensionStateWord),
			}
			if k == other {
				metric.Oracle = domain.Address{8}
			}
			if err := store.Upsert(pool.State{Key: k, Token0: domain.Address{1}, Token1: domain.Address{2}, Metric: metric}); err != nil {
				t.Fatal(err)
			}
		}
		store.ClearDirty()
		routes, err := newRouting([]domain.PoolKey{key, other})
		if err != nil {
			t.Fatal(err)
		}
		word := metricHash(t, sample.OracleWord)
		tx := Transaction{Hash: domain.Hash{1}, Successful: true, Raw: rawCall(t, oracle, append([]byte{0}, word[:]...)),
			Logs: []Log{{Address: oracle, Topics: []domain.Hash{metricomm.TopicOracleUpdated, {1}}}}}
		ref := domain.BlockRef{Number: testBlockNumber, Hash: testHash, Timestamp: sample.BlockTimestamp}
		dirty, err := applyTransaction(context.Background(), store, routes, ref, tx)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := store.Pool(key)
		if len(dirty) != 1 || dirty[0] != key || got.Metric.BidX64 == nil || got.Metric.BidX64.String() != sample.ExtensionBidX64 || got.Metric.AskX64.String() != sample.ExtensionAskX64 {
			t.Fatalf("%s: dirty %v, quote [%v, %v], chain [%s, %s]", sample.Pool, dirty, got.Metric.BidX64, got.Metric.AskX64, sample.ExtensionBidX64, sample.ExtensionAskX64)
		}
		if untouched, _ := store.Pool(other); untouched.Metric.BidX64 != nil {
			t.Fatal("a pool of another oracle was quoted")
		}

		// The same update without its calldata (a block read as receipts)
		// must not leave the previous quote standing.
		tx.Raw, tx.Hash = nil, domain.Hash{2}
		ref.Number++
		if _, err := applyTransaction(context.Background(), store, routes, ref, tx); err != nil {
			t.Fatal(err)
		}
		if got, _ := store.Pool(key); got.Metric.BidX64 != nil || got.Metric.AskX64 != nil {
			t.Fatal("an update without calldata kept the old quote")
		}
	}
}

// Rival arbitrages through a MetricOmm pool come in bursts where the pool is
// swapped again and again; holding the pool after a swap until its storage is
// read a block later leaves no candidate for any of them. The swap is
// replayed onto the words
// exactly; the quote extension's own state after it is not known, so the
// quote is priced again at the pool's new position with the state it had
// (a few tens of ppm at most, SkewApprox) and the pool stays quotable.
func TestMetricPoolSwapsReplayAndRepriceThePool(t *testing.T) {
	var fixture struct {
		Immutables struct {
			Scale0  string `json:"token0_scale_multiplier"`
			Scale1  string `json:"token1_scale_multiplier"`
			Lowest  int16  `json:"lowest_bin"`
			Highest int16  `json:"highest_bin"`
		} `json:"immutables"`
		Cases []struct {
			WordsBefore []string `json:"words_before"`
			Logs        []struct {
				Topics []string `json:"topics"`
				Data   string   `json:"data"`
			} `json:"logs"`
			WordsChanged map[string]string `json:"words_changed"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile("../dex/metricomm/testdata/0x2b4a9de7bdaffea997f5e12d0ebbffddf6aea3d3-events.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&fixture); err != nil {
		t.Skipf("fixture layout: %v", err)
	}
	key := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: domain.Address{0x2b}}
	scale0, _ := new(big.Int).SetString(fixture.Immutables.Scale0, 0)
	scale1, _ := new(big.Int).SetString(fixture.Immutables.Scale1, 0)
	swaps := 0
	for _, c := range fixture.Cases {
		if len(c.Logs) == 0 || c.Logs[0].Topics[0] != metricomm.TopicBinSwapped.Hex() {
			continue
		}
		swaps++
		metric := &pool.MetricState{Token0Scale: scale0, Token1Scale: scale1, LowestBin: fixture.Immutables.Lowest, HighestBin: fixture.Immutables.Highest,
			Price1e8: 208_000_000_000, BidSpreadPpm: 200, AskSpreadPpm: 200, BidX64: big.NewInt(1), AskX64: big.NewInt(2)}
		for _, w := range c.WordsBefore {
			metric.Words = append(metric.Words, metricHash(t, w))
		}
		store := state.New()
		if err := store.Upsert(pool.State{Key: key, Token0: domain.Address{1}, Token1: domain.Address{2}, Metric: metric}); err != nil {
			t.Fatal(err)
		}
		store.ClearDirty()
		var logs []Log
		for _, l := range c.Logs {
			log := Log{Address: key.Address}
			for _, topic := range l.Topics {
				log.Topics = append(log.Topics, metricHash(t, topic))
			}
			log.Data, _ = hex.DecodeString(strings.TrimPrefix(l.Data, "0x"))
			logs = append(logs, log)
		}
		dirty := applyLogs(t, store, []domain.PoolKey{key}, logs...)
		got, _ := store.Pool(key)
		if len(dirty) != 1 || got.Metric.Stale || !got.Metric.SkewApprox || got.UpdatedBlock != testBlockNumber {
			t.Fatalf("dirty %v stale %v approx %v block %d", dirty, got.Metric.Stale, got.Metric.SkewApprox, got.UpdatedBlock)
		}
		bidSkew, askSkew := metricomm.SkewPpm(got.Metric.Words[0], got.Metric.SkewState, metricomm.DecodeSkewConfig(got.Metric.SkewConfig, got.Metric.SkewParameters), testRef().Timestamp)
		bid, ask := metricomm.Quote(208_000_000_000, 200+bidSkew, 200+askSkew)
		if got.Metric.BidX64.Cmp(bid) != 0 || got.Metric.AskX64.Cmp(ask) != 0 {
			t.Fatalf("quote [%s, %s] after the swap, want it priced at the new position [%s, %s]", got.Metric.BidX64, got.Metric.AskX64, bid, ask)
		}
		for index, want := range c.WordsChanged {
			var i int
			if _, err := json.Number(index).Int64(); err != nil {
				t.Fatal(err)
			}
			n, _ := json.Number(index).Int64()
			i = int(n)
			if got.Metric.Words[i] != metricHash(t, want) {
				t.Fatalf("word %d: %s, chain %s", i, got.Metric.Words[i].Hex(), want)
			}
		}
	}
	if swaps < 20 {
		t.Fatalf("%d swap blocks replayed", swaps)
	}
}

// An event the replay does not model still holds the pool until it is read.
func TestMetricPoolEventsOutsideTheReplayHoldThePool(t *testing.T) {
	key := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: domain.Address{0x2b}}
	metric := &pool.MetricState{Token0Scale: big.NewInt(1), Token1Scale: big.NewInt(1), Words: []domain.Hash{{}, {}, {}}, BidX64: big.NewInt(1), AskX64: big.NewInt(2)}
	store := state.New()
	if err := store.Upsert(pool.State{Key: key, Token0: domain.Address{1}, Token1: domain.Address{2}, Metric: metric}); err != nil {
		t.Fatal(err)
	}
	store.ClearDirty()
	// LiquidityRemoved, as on the WETH/USDC pool at block 523018237.
	removed := metricHash(t, "0xf53d04bdbda6d88dd536895ec533833d3e09d0cbe2474fae32a368c5f83fc610")
	applyLogs(t, store, []domain.PoolKey{key}, Log{Address: key.Address, Topics: []domain.Hash{removed}})
	if got, _ := store.Pool(key); !got.Metric.Stale {
		t.Fatal("a liquidity event left the pool quotable")
	}
}

// A send quoted from the extension's quote alone fills 1 to 7 bps worse,
// because the price provider holds the quote to a second oracle. An update of
// that oracle
// reaches the pools registered with it from the transaction's calldata: the
// ask follows a reference above the extension's quote, and an update whose
// calldata was not seen takes the quote away.
func TestReferenceUpdateTransactionMovesTheQuoteOfItsPools(t *testing.T) {
	var fixture struct {
		Input string `json:"input"`
	}
	raw, err := os.ReadFile("../dex/metricomm/testdata/reference-update.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	input, err := hex.DecodeString(strings.TrimPrefix(fixture.Input, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	reference := domain.Address{7}
	key := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: domain.Address{5}}
	store := state.New()
	// The feed prices ETH at $1,900; the update's reference is $1,933.41.
	metric := &pool.MetricState{Token0Scale: big.NewInt(1), Token1Scale: big.NewInt(1), Words: []domain.Hash{{}, {}, {}},
		Oracle: domain.Address{8}, Price1e8: 190_000_000_000, BidSpreadPpm: 100, AskSpreadPpm: 100,
		Reference: reference, ReferenceBaseFeed: 2, ReferenceQuoteFeed: 7}
	if err := store.Upsert(pool.State{Key: key, Token0: domain.Address{1}, Token1: domain.Address{2}, Metric: metric}); err != nil {
		t.Fatal(err)
	}
	store.ClearDirty()
	routes, err := newRouting([]domain.PoolKey{key})
	if err != nil {
		t.Fatal(err)
	}
	logs := []Log{{Address: reference, Topics: []domain.Hash{metricomm.TopicOracleUpdated, {31: 1}}}, {Address: reference, Topics: []domain.Hash{metricomm.TopicOracleUpdated, {31: 2}}}}
	tx := Transaction{Hash: domain.Hash{1}, Successful: true, Raw: rawCall(t, reference, input), Logs: logs}
	ref := domain.BlockRef{Number: testBlockNumber, Hash: testHash, Timestamp: 1}
	dirty, err := applyTransaction(context.Background(), store, routes, ref, tx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := store.Pool(key)
	_, wantAsk := metricomm.ReferenceBand(193_286_157_120, 99_971_843)
	extensionBid, _ := metricomm.Quote(190_000_000_000, 100, 100)
	if len(dirty) != 1 || got.Metric.ReferenceBase1e8 != 193_286_157_120 || got.Metric.ReferenceQuote1e8 != 99_971_843 ||
		got.Metric.AskX64 == nil || got.Metric.AskX64.Cmp(wantAsk) != 0 || got.Metric.BidX64.Cmp(extensionBid) != 0 {
		t.Fatalf("dirty %v, feeds %d/%d, quote [%v, %v], want ask %s", dirty, got.Metric.ReferenceBase1e8, got.Metric.ReferenceQuote1e8, got.Metric.BidX64, got.Metric.AskX64, wantAsk)
	}

	tx.Raw, tx.Hash = nil, domain.Hash{2}
	ref.Number++
	if _, err := applyTransaction(context.Background(), store, routes, ref, tx); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Pool(key); got.Metric.BidX64 != nil || got.Metric.ReferenceBase1e8 != 0 || got.Metric.Price1e8 == 0 {
		t.Fatal("a reference update without calldata kept the old quote, or dropped the pool's own feed")
	}
}
