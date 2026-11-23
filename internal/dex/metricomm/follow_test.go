package metricomm

import (
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// A swapped pool is followed without a storage read: the words by replay and
// the quote extension's state by its afterSwap rule (NextSkewState). Over
// every fixture block (Arbitrum One, both pools, single and repeated swaps)
// the followed state must equal the chain's after the block.
func TestFollowLogCarriesTheExtensionState(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for _, entry := range entries {
		if !isSkewUpdates(entry.Name()) {
			continue
		}
		fixture := loadJSON[skewUpdateFixture](t, entry.Name())
		scale0, ok0 := new(big.Int).SetString(fixture.Immutables.Token0ScaleMultiplier, 0)
		scale1, ok1 := new(big.Int).SetString(fixture.Immutables.Token1ScaleMultiplier, 0)
		if !ok0 || !ok1 {
			t.Fatalf("%s: scale multipliers", entry.Name())
		}
		for _, c := range fixture.Cases {
			metric := &pool.MetricState{Token0Scale: scale0, Token1Scale: scale1, LowestBin: fixture.Immutables.LowestBin, HighestBin: fixture.Immutables.HighestBin,
				SkewConfig: mustHash(t, fixture.ConfigWord), SkewParameters: mustHash(t, fixture.ParametersWord), SkewState: mustHash(t, c.StateBefore),
				Price1e8: 208_000_000_000, BidSpreadPpm: 200, AskSpreadPpm: 200}
			for _, w := range c.WordsBefore {
				metric.Words = append(metric.Words, mustHash(t, w))
			}
			for _, log := range c.Logs {
				topics := make([]domain.Hash, len(log.Topics))
				for i, topic := range log.Topics {
					topics[i] = mustHash(t, topic)
				}
				FollowLog(metric, topics, mustBytes(t, log.Data), c.BlockTimestamp)
			}
			if metric.Stale || metric.Words[0] != mustHash(t, c.Slot0After) {
				t.Fatalf("%s block %d: stale %v, slot 0 %s, chain %s", entry.Name(), c.BlockNumber, metric.Stale, metric.Words[0].Hex(), c.Slot0After)
			}
			if got := metric.SkewState.Hex(); !strings.EqualFold(got, c.StateAfter) {
				t.Fatalf("%s block %d: extension state %s, chain %s", entry.Name(), c.BlockNumber, got, c.StateAfter)
			}
			bidSkew, askSkew := SkewPpm(metric.Words[0], metric.SkewState, DecodeSkewConfig(metric.SkewConfig, metric.SkewParameters), c.BlockTimestamp)
			bid, ask := Quote(metric.Price1e8, metric.BidSpreadPpm+bidSkew, metric.AskSpreadPpm+askSkew)
			if metric.BidX64 == nil || metric.BidX64.Cmp(bid) != 0 || metric.AskX64.Cmp(ask) != 0 {
				t.Fatalf("%s block %d: quote not priced at the followed state", entry.Name(), c.BlockNumber)
			}
			blocks++
		}
	}
	if blocks < 150 {
		t.Fatalf("%d blocks followed", blocks)
	}
}
