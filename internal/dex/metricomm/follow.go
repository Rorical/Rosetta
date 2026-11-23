package metricomm

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// TopicOracleUpdated is the oracle's event for one updated feed. It carries
// the feed id and no price: the price is in the update's calldata.
var TopicOracleUpdated = domain.Hash{
	0x29, 0xe0, 0x88, 0xdf, 0xfd, 0x7b, 0xf6, 0x81, 0x5d, 0x08, 0x81, 0x37, 0xa5, 0x9f, 0x36, 0x4d,
	0x67, 0x47, 0x16, 0x3b, 0xaa, 0x7f, 0xf4, 0xbc, 0x47, 0xf3, 0x16, 0xa1, 0xad, 0x1b, 0x95, 0x86,
}

// FollowLog applies one log of the pool, seen in a block with the given
// timestamp, to its state and reports whether the state changed. A swap is
// replayed onto the words exactly, the quote extension's state follows by
// its afterSwap rule (NextSkewState, once per Swap event) and the quote is
// priced again at the new position. SkewApprox stays set until the next
// storage read: a Fast view knows a pending block's time only to its parent's
// second, and the read is the check on the rule. An event the replay does
// not model, or a log that does not decode, leaves the pool stale.
func FollowLog(metric *pool.MetricState, topics []domain.Hash, data []byte, blockTime uint64) bool {
	immutables, err := immutablesOf(metric)
	if err != nil {
		return markStale(metric)
	}
	next, changed, stale, err := ApplyLog(metric.Words, immutables, topics, data)
	if err != nil || stale {
		return markStale(metric)
	}
	if !changed {
		return false
	}
	metric.Words = next
	if topics[0] == TopicSwap {
		metric.SkewState = NextSkewState(metric.SkewState, next[0], metric.SkewConfig, metric.SkewParameters, blockTime)
	}
	metric.SkewApprox = true
	price(metric, blockTime)
	return true
}

func markStale(metric *pool.MetricState) bool {
	if metric.Stale {
		return false
	}
	metric.Stale = true
	return true
}

// FollowOracle sets the pool's quote from an oracle update seen in a block
// with the given timestamp. A feed without a usable price clears the quote.
func FollowOracle(metric *pool.MetricState, update OracleUpdate, blockTime uint64) {
	metric.QuoteMS, metric.OracleWord = update.TimestampMS, update.Word
	metric.Price1e8, metric.BidSpreadPpm, metric.AskSpreadPpm = 0, 0, 0
	if int(metric.FeedIndex) < len(update.Feeds) {
		feed := update.Feeds[metric.FeedIndex]
		metric.Price1e8, metric.BidSpreadPpm, metric.AskSpreadPpm = feed.Price1e8, feed.BidSpreadPpm, feed.AskSpreadPpm
	}
	price(metric, blockTime)
}

// price sets the quote from the last feed, widened by the extension for the
// pool's position at blockTime and held to the provider's reference band.
func price(metric *pool.MetricState, blockTime uint64) {
	metric.BidX64, metric.AskX64 = nil, nil
	if metric.Price1e8 == 0 || len(metric.Words) == 0 {
		return
	}
	bidSkew, askSkew := SkewPpm(metric.Words[0], metric.SkewState, DecodeSkewConfig(metric.SkewConfig, metric.SkewParameters), blockTime)
	metric.BidX64, metric.AskX64 = Quote(metric.Price1e8, metric.BidSpreadPpm+bidSkew, metric.AskSpreadPpm+askSkew)
	clampToReference(metric)
}

// ForgetQuote clears the quote after an oracle update whose calldata was not
// seen: the old quote is no longer the chain's.
func ForgetQuote(metric *pool.MetricState) {
	metric.BidX64, metric.AskX64 = nil, nil
	metric.Price1e8, metric.BidSpreadPpm, metric.AskSpreadPpm = 0, 0, 0
	metric.OracleWord = domain.Hash{}
}
