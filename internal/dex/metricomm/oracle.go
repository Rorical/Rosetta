package metricomm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
	"math/big"
)

// ErrInvalidOracleUpdate reports calldata that is not a price update of the
// oracle the pools' quotes are derived from.
var ErrInvalidOracleUpdate = errors.New("metricomm: invalid oracle update")

// OracleFeedCount is the number of feeds one oracle update carries. On
// Arbitrum One they are USDC, ETH and BTC, in that order.
const OracleFeedCount = 3

// oracleUpdateLength is the size of an update's calldata. It has no selector:
// one zero byte, then the 32 bytes the oracle stores as a single word.
const oracleUpdateLength = 33

// FeedUpdate is one feed of an oracle update.
type FeedUpdate struct {
	// Price1e8 is the price in USD with 8 decimals. Zero means the feed
	// carries no price.
	Price1e8 uint64
	// BidSpreadPpm and AskSpreadPpm are the distances of the bid below and
	// the ask above the price, in millionths of the price.
	BidSpreadPpm uint32
	AskSpreadPpm uint32
}

// OracleUpdate is a decoded update transaction.
type OracleUpdate struct {
	Feeds [OracleFeedCount]FeedUpdate
	// TimestampMS is the time the update states for itself, in Unix
	// milliseconds. The quote is only valid on chain within a window around
	// the block time, so callers compare it with their own clock.
	TimestampMS uint64
	// Word is what the oracle stores for the update: its calldata without
	// the leading byte.
	Word domain.Hash
}

// DecodeOracleUpdate decodes the calldata of an update transaction:
//
//	byte  0       zero
//	bytes 1..18   three feeds of 6 bytes: a 4-byte price word (top 5 bits a
//	              left shift, low 27 bits a mantissa), a bid spread code and
//	              an ask spread code
//	bytes 19..23  zero
//	bytes 24..31  timestamp, Unix milliseconds, big endian
//	byte  32      zero
//
// Anything else is rejected, including a non-zero byte where every observed
// update has zero: such an update uses a part of the format this decoder
// does not know.
func DecodeOracleUpdate(input []byte) (OracleUpdate, error) {
	if len(input) != oracleUpdateLength {
		return OracleUpdate{}, fmt.Errorf("%w: %d bytes, want %d", ErrInvalidOracleUpdate, len(input), oracleUpdateLength)
	}
	if input[0] != 0 || input[32] != 0 {
		return OracleUpdate{}, fmt.Errorf("%w: non-zero framing byte", ErrInvalidOracleUpdate)
	}
	for _, reserved := range input[19:24] {
		if reserved != 0 {
			return OracleUpdate{}, fmt.Errorf("%w: non-zero reserved byte", ErrInvalidOracleUpdate)
		}
	}
	var update OracleUpdate
	for i := range update.Feeds {
		feed := input[1+6*i : 7+6*i]
		priceWord := binary.BigEndian.Uint32(feed)
		update.Feeds[i] = FeedUpdate{
			// At most 27+31 bits, so the shift cannot overflow.
			Price1e8:     uint64(priceWord&(1<<27-1)) << (priceWord >> 27),
			BidSpreadPpm: spreadPpm(feed[4]),
			AskSpreadPpm: spreadPpm(feed[5]),
		}
	}
	update.TimestampMS = binary.BigEndian.Uint64(input[24:32])
	copy(update.Word[:], input[1:33])
	return update, nil
}

// spreadPpm expands a one-byte spread code. The steps widen with the code:
// 1 ppm up to 9, then 2, 5, 10 and 150 ppm per code, capped at 1%. The table
// was read out of the deployed oracle for all 256 codes (testdata/oracle-codes.json).
func spreadPpm(code uint8) uint32 {
	c := uint32(code)
	switch {
	case c < 10:
		return c
	case c < 64:
		return 2*c - 8
	case c < 128:
		return 5*c - 197
	case c < 192:
		return 10*c - 832
	default:
		return min(150*c-27572, 10_000)
	}
}

var (
	quoteDenominator = big.NewInt(100_000_000 * 1_000_000) // price decimals times ppm
	bigOne           = big.NewInt(1)
)

// Quote turns a feed's price and spreads into the bid and ask the pool's
// quote extension derives from them, Q64.64 in whole tokens:
//
//	bid = floor(price * (1e6 - bidSpread) * 2^64 / 1e14)
//	ask = ceil (price * (1e6 + askSpread) * 2^64 / 1e14)
//
// The price is the feed's own; the USDC feed does not enter it. It returns
// nil, nil when the inputs give no positive bid.
//
// The extension adds SkewPpm to a spread before this step, so pass the sum
// to get its quote to the last bit. The price provider then holds the result
// to its reference band (ReferenceBand).
func Quote(price1e8 uint64, bidSpreadPpm, askSpreadPpm uint32) (bidX64, askX64 *big.Int) {
	if price1e8 == 0 || bidSpreadPpm >= 1_000_000 {
		return nil, nil
	}
	price := new(big.Int).SetUint64(price1e8)
	bidX64 = new(big.Int).Mul(price, big.NewInt(int64(1_000_000-bidSpreadPpm)))
	bidX64.Lsh(bidX64, 64).Quo(bidX64, quoteDenominator)
	if bidX64.Sign() == 0 {
		return nil, nil
	}
	askX64 = new(big.Int).Mul(price, big.NewInt(1_000_000+int64(askSpreadPpm)))
	askX64.Lsh(askX64, 64).Add(askX64, quoteDenominator).Sub(askX64, bigOne).Quo(askX64, quoteDenominator)
	return bidX64, askX64
}
