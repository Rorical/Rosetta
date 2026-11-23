package metricomm

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// immutablesOutput lays a fixture's immutables out as getImmutables()
// answers, by the word positions the fixture script read them from.
func immutablesOutput(t *testing.T, token0, token1 domain.Address, im fixtureImmutables, extensions ...domain.Address) []byte {
	t.Helper()
	output := make([]byte, immutablesWords*32)
	copy(output[1*32+12:], token0[:])
	copy(output[2*32+12:], token1[:])
	mustBig(t, im.Token0ScaleMultiplier).FillBytes(output[3*32 : 4*32])
	mustBig(t, im.Token1ScaleMultiplier).FillBytes(output[4*32 : 5*32])
	for i, bin := range []int16{im.LowestBin, im.HighestBin} {
		value := big.NewInt(int64(bin))
		if bin < 0 {
			value.Add(value, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		value.FillBytes(output[(9+i)*32 : (10+i)*32])
	}
	for i, extension := range extensions {
		copy(output[(11+i)*32+12:], extension[:])
	}
	return output
}

func TestRegisterReadsImmutablesAndStartsStale(t *testing.T) {
	f := loadFixtures[liveFixture](t, isLive)[0]
	extension, other := domain.Address{0xf1}, domain.Address{0xe4}
	registration := pool.MetricState{Oracle: domain.Address{0xe8}, FeedIndex: 1, OracleSlot: domain.Hash{1}, QuoteExtension: extension, QuoteExtensionSlot: domain.Hash{2}}
	output := immutablesOutput(t, testToken0, testToken1, f.Immutables, other, extension)

	metric, token0, token1, err := Register(registration, output)
	if err != nil {
		t.Fatal(err)
	}
	if token0 != testToken0 || token1 != testToken1 || metric.LowestBin != f.Immutables.LowestBin || metric.HighestBin != f.Immutables.HighestBin ||
		metric.Token0Scale.String() != f.Immutables.Token0ScaleMultiplier || metric.Token1Scale.String() != f.Immutables.Token1ScaleMultiplier {
		t.Fatalf("registered %+v for %s/%s", metric, token0, token1)
	}
	if !metric.Stale || metric.Words != nil || metric.BidX64 != nil || metric.Oracle != registration.Oracle || metric.QuoteExtensionSlot != registration.QuoteExtensionSlot {
		t.Fatalf("a registration must start stale and keep its configuration: %+v", metric)
	}
	// The record slot belongs to one extension; a pool that does not list it
	// would have its quote computed from another contract's storage.
	if _, _, _, err := Register(registration, immutablesOutput(t, testToken0, testToken1, f.Immutables, other)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unlisted quote extension: %v", err)
	}
	if _, _, _, err := Register(registration, output[:len(output)-32]); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("short answer: %v", err)
	}
}

// TestRefreshGivesTheChainsQuote loads a pool from storage words the way the
// runtime reads them and requires the quote the chain's own extension
// returned in the same block, for every sample of both pools. The bin words
// come from the pool's pinned fixture and slot 0, the extension's state word
// and the oracle's word from the quote sample, since no single fixture holds
// all of them at one block; the quote depends on slot 0 alone.
func TestRefreshGivesTheChainsQuote(t *testing.T) {
	quotes := loadJSON[quoteFixture](t, "quotes.json")
	live := map[string]liveFixture{}
	for _, f := range loadFixtures[liveFixture](t, isLive) {
		live[f.Pool] = f
	}
	checked := 0
	for _, sample := range quotes.Samples {
		f, ok := live[sample.Pool]
		if !ok {
			t.Fatalf("no pinned fixture for %s", sample.Pool)
		}
		state := fixturePoolState(t, f.Immutables, f.Storage, nil, nil)
		metric := Unloaded(state.Metric)
		metric.FeedIndex = uint8(sample.FeedIndex)
		data, count, err := WordsCall(metric)
		if err != nil || count != len(state.Metric.Words) || len(data) != 4+64+32*count {
			t.Fatalf("words call of %d bytes for %d words: %v", len(data), count, err)
		}
		words := append([]domain.Hash{mustHash(t, sample.Slot0)}, state.Metric.Words[1:]...)
		answer := append(append(abiWord(32), abiWord(uint64(count))...), flatten(words)...)
		decoded, err := DecodeWords(answer, count)
		if err != nil {
			t.Fatal(err)
		}
		extension := quotes.QuoteExtension[sample.Pool]
		skew := [3]domain.Hash{mustHash(t, extension.ConfigWord), mustHash(t, extension.ParametersWord), mustHash(t, sample.ExtensionStateWord)}
		if err := Refresh(metric, decoded, skew, mustHash(t, sample.OracleWord), sample.BlockTimestamp); err != nil {
			t.Fatalf("block %d: %v", sample.BlockNumber, err)
		}
		if metric.Stale || metric.BidX64 == nil || metric.BidX64.String() != sample.ExtensionBidX64 || metric.AskX64.String() != sample.ExtensionAskX64 {
			t.Fatalf("block %d %s: quote [%v, %v], chain [%s, %s]", sample.BlockNumber, sample.Pool, metric.BidX64, metric.AskX64, sample.ExtensionBidX64, sample.ExtensionAskX64)
		}
		if metric.QuoteMS != sample.Feeds[0].TimestampMS {
			t.Fatalf("block %d: quote time %d, chain %d", sample.BlockNumber, metric.QuoteMS, sample.Feeds[0].TimestampMS)
		}
		checked++
	}
	if checked < 40 {
		t.Fatalf("%d samples", checked)
	}

	// Words of another grid are refused and leave the pool as it was.
	state := fixturePoolState(t, live[quotes.Samples[0].Pool].Immutables, live[quotes.Samples[0].Pool].Storage, nil, nil)
	metric := Unloaded(state.Metric)
	if err := Refresh(metric, state.Metric.Words[:len(state.Metric.Words)-1], [3]domain.Hash{}, domain.Hash{}, 1); !errors.Is(err, ErrInvalidState) || !metric.Stale || metric.Words != nil {
		t.Fatalf("short read accepted: %v, %+v", err, metric)
	}
	if _, err := DecodeWords(make([]byte, 64+32), 2); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("short extsload answer: %v", err)
	}
}

func flatten(words []domain.Hash) []byte {
	out := make([]byte, 0, 32*len(words))
	for _, word := range words {
		out = append(out, word[:]...)
	}
	return out
}

func TestSkewSlotsFollowTheRecord(t *testing.T) {
	base := mustHash(t, "0xd23040c859ccb3f30ff05a5fa4ef2f6317f4a5baf1ef9c66778e0a50a92baada")
	slots := SkewSlots(&pool.MetricState{QuoteExtensionSlot: base})
	want := [3]string{
		"0xd23040c859ccb3f30ff05a5fa4ef2f6317f4a5baf1ef9c66778e0a50a92baada",
		"0xd23040c859ccb3f30ff05a5fa4ef2f6317f4a5baf1ef9c66778e0a50a92baae3",
		"0xd23040c859ccb3f30ff05a5fa4ef2f6317f4a5baf1ef9c66778e0a50a92baae4",
	}
	for i, slot := range slots {
		if slot.Hex() != want[i] {
			t.Fatalf("slot %d is %s, want %s", i, slot.Hex(), want[i])
		}
	}
}

// A screening model that is a product curve over the pool's whole inventory
// overstates slippage by an order of magnitude (about 35 bps for a $200 trade
// against 57,759 USDC), so the search never ranks routes rivals do take; the
// pool's price moves by one bin length (1–2 bps) per bin of liquidity. The
// model must stay within 2 bps of the exact quote for
// trades of a thousandth to a fiftieth of the pool, in both directions.
func TestRankingModelTracksTheAdapter(t *testing.T) {
	adapter := New()
	for _, f := range loadFixtures[liveFixture](t, isLive) {
		bid, ask := mustBig(t, f.Cases[0].BidX64), mustBig(t, f.Cases[0].AskX64)
		state := fixturePoolState(t, f.Immutables, f.Storage, bid, ask)
		model, err := RankingModel(state)
		if err != nil {
			t.Fatal(err)
		}
		if model.ConstantProduct {
			t.Fatal("the model must not take part in constant-product proofs")
		}
		decoded, err := decodeMetricState(state.Metric)
		if err != nil {
			t.Fatal(err)
		}
		held0 := new(big.Int).Quo(decoded.TotalToken0Scaled.ToBig(), state.Metric.Token0Scale)
		held1 := new(big.Int).Quo(decoded.TotalToken1Scaled.ToBig(), state.Metric.Token1Scale)
		for _, direction := range []struct {
			name      string
			token     domain.Address
			in, out   *big.Int
			inventory *big.Int // of the output token
		}{{"sell", testToken0, model.Reserve0, model.Reserve1, held1}, {"buy", testToken1, model.ReverseIn, model.ReverseOut, held0}} {
			for _, share := range []int64{1000, 200, 50} {
				// The input that buys about inventory/share of the output.
				amount := new(big.Int).Mul(direction.inventory, direction.in)
				amount.Quo(amount, new(big.Int).Mul(direction.out, big.NewInt(share)))
				exact, err := adapter.QuoteExactIn(context.Background(), state, pool.DeepState{}, direction.token, amount)
				if err != nil {
					t.Fatalf("%s %s 1/%d: %v", f.Pool, direction.name, share, err)
				}
				modelled := new(big.Int).Mul(amount, direction.out)
				modelled.Quo(modelled, new(big.Int).Add(direction.in, amount))
				difference := new(big.Int).Abs(new(big.Int).Sub(modelled, exact))
				if difference.Mul(difference, big.NewInt(5000)).Cmp(exact) > 0 {
					t.Errorf("%s %s 1/%d of the pool: modelled %s, exact %s for %s in", f.Pool, direction.name, share, modelled, exact, amount)
				}
			}
		}
		if fee, err := SwapFee(state, testToken0); err != nil || fee != 0 {
			t.Fatalf("%s: fee %d, %v: the pinned pools charge no notional or bin fee", f.Pool, fee, err)
		}

		stale := state.Copy()
		stale.Metric.Stale = true
		if _, err := RankingModel(stale); !errors.Is(err, ErrStaleState) {
			t.Fatalf("stale pool modelled: %v", err)
		}
		unquoted := state.Copy()
		unquoted.Metric.BidX64, unquoted.Metric.AskX64 = nil, nil
		if _, err := RankingModel(unquoted); !errors.Is(err, ErrNoQuote) {
			t.Fatalf("unquoted pool modelled: %v", err)
		}
	}
}
