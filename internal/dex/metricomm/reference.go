package metricomm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// The pool's price provider does not pass the quote extension's quote on
// unchanged. It reads a second oracle, the reference: the price of the pool's
// base token over the price of its quote token, two feeds of a signed price
// stream that anyone posts about every 0.4 s. A side of the quote that would
// trade through the reference is moved 5 ppm past it:
//
//	bid = min(extension bid, reference - 5 ppm)
//	ask = max(extension ask, reference + 5 ppm)
//
// The clamp binds exactly when the extension's quote looks mispriced against
// the market, which is when a route through the pool looks profitable; a
// quote without it fills 1 to 7 bps worse. The pool's source is not verified,
// so the rule is fitted to testdata/reference-quotes.json.

// ErrInvalidReferenceUpdate reports calldata that is not a price update of
// the reference oracle in the form this decoder knows.
var ErrInvalidReferenceUpdate = errors.New("metricomm: invalid reference update")

const (
	referenceEnvelopeMagic = 0x2a22999a
	referencePayloadMagic  = 0x93c7d375
	// referenceExponent is the only price exponent seen: prices are USD
	// with 8 decimals, as the stored word holds them.
	referenceExponent = -8
	// referenceMappingSlot is the oracle's mapping from feed id to its word.
	referenceMappingSlot = 1
)

// ReferenceFeed is one feed of a reference update.
type ReferenceFeed struct {
	ID       uint32
	Price1e8 uint64
}

// ReferenceUpdate is a decoded update of the reference oracle.
type ReferenceUpdate struct {
	// TimestampMS is the update's own time. The oracle stores milliseconds.
	TimestampMS uint64
	Feeds       []ReferenceFeed
}

// DecodeReferenceUpdate decodes the calldata of a reference update:
//
//	u16 feed count, then that many u32 feed ids
//	u32 0x2a22999a, a 65-byte signature, u16 payload length
//	payload: u32 0x93c7d375, u64 timestamp in microseconds, u8 channel,
//	         u8 feed count, then per feed a u32 id, u8 property count and
//	         the properties: 0 price (i64), 4 exponent (i16), 5 confidence
//	         (u64), 12 feed timestamp (u8 flag, u64 when set)
//
// The signature is not checked: an update is applied only from a successful
// transaction to the oracle, which checked it. Any other property, exponent
// or framing is rejected rather than guessed.
func DecodeReferenceUpdate(input []byte) (ReferenceUpdate, error) {
	fail := func(what string) (ReferenceUpdate, error) {
		return ReferenceUpdate{}, fmt.Errorf("%w: %s", ErrInvalidReferenceUpdate, what)
	}
	if len(input) < 2 {
		return fail("no header")
	}
	at := 2 + 4*int(binary.BigEndian.Uint16(input))
	if len(input) < at+4+65+2 || binary.BigEndian.Uint32(input[at:]) != referenceEnvelopeMagic {
		return fail("no signed envelope")
	}
	at += 4 + 65
	payload := input[at+2:]
	if int(binary.BigEndian.Uint16(input[at:])) != len(payload) || len(payload) < 14 || binary.BigEndian.Uint32(payload) != referencePayloadMagic {
		return fail("payload framing")
	}
	update := ReferenceUpdate{TimestampMS: binary.BigEndian.Uint64(payload[4:]) / 1000}
	feeds := int(payload[13])
	payload = payload[14:]
	for range feeds {
		if len(payload) < 5 {
			return fail("truncated feed")
		}
		feed := ReferenceFeed{ID: binary.BigEndian.Uint32(payload)}
		properties := int(payload[4])
		payload = payload[5:]
		exponent, priced := false, false
		for range properties {
			if len(payload) == 0 {
				return fail("truncated property")
			}
			id := payload[0]
			size := 0
			switch id {
			case 0, 5:
				size = 8
			case 4:
				size = 2
			case 12:
				// A flag byte, then the timestamp when it is set.
				size = 1
				if len(payload) > 1 && payload[1] == 1 {
					size = 9
				}
			}
			if size == 0 || len(payload) < 1+size {
				return fail("unknown or truncated property")
			}
			value := payload[1 : 1+size]
			switch id {
			case 0:
				price := int64(binary.BigEndian.Uint64(value))
				if price <= 0 {
					return fail("price not positive")
				}
				feed.Price1e8, priced = uint64(price), true
			case 4:
				if int16(binary.BigEndian.Uint16(value)) != referenceExponent {
					return fail("exponent")
				}
				exponent = true
			case 12:
				if value[0] > 1 {
					return fail("feed timestamp flag")
				}
			}
			payload = payload[1+size:]
		}
		if !exponent || !priced {
			return fail("feed without price or exponent")
		}
		update.Feeds = append(update.Feeds, feed)
	}
	if len(payload) != 0 {
		return fail("trailing bytes")
	}
	return update, nil
}

// ReferenceSlot is the reference oracle's storage slot for a feed.
func ReferenceSlot(feed uint32) domain.Hash {
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write(abiWord(uint64(feed)))
	hasher.Write(abiWord(referenceMappingSlot))
	var slot domain.Hash
	copy(slot[:], hasher.Sum(nil))
	return slot
}

// DecodeReferenceWord reads a feed's stored word: the price in the low 8
// bytes and the update's millisecond timestamp in bytes 12 to 19.
func DecodeReferenceWord(word domain.Hash) (price1e8, timestampMS uint64) {
	return binary.BigEndian.Uint64(word[24:]), binary.BigEndian.Uint64(word[12:20])
}

// FollowReference applies an update of the reference oracle to the pool's
// two feeds and prices the quote again. The oracle keeps a feed's newer
// value, so an older update leaves it.
func FollowReference(metric *pool.MetricState, update ReferenceUpdate, blockTime uint64) {
	for _, feed := range update.Feeds {
		if feed.ID == metric.ReferenceBaseFeed && update.TimestampMS > metric.ReferenceBaseMS {
			metric.ReferenceBase1e8, metric.ReferenceBaseMS = feed.Price1e8, update.TimestampMS
		}
		if feed.ID == metric.ReferenceQuoteFeed && update.TimestampMS > metric.ReferenceQuoteMS {
			metric.ReferenceQuote1e8, metric.ReferenceQuoteMS = feed.Price1e8, update.TimestampMS
		}
	}
	price(metric, blockTime)
}

// FollowReferenceWords sets the pool's two reference feeds from the oracle's
// stored words, read at a block. It does not price the quote: the caller
// follows with the oracle's word or a full refresh.
func FollowReferenceWords(metric *pool.MetricState, base, quote domain.Hash) {
	metric.ReferenceBase1e8, metric.ReferenceBaseMS = DecodeReferenceWord(base)
	metric.ReferenceQuote1e8, metric.ReferenceQuoteMS = DecodeReferenceWord(quote)
}

// ForgetReference clears the reference after an update whose calldata was not
// seen or not understood: the pool has no quote until its feeds are read.
func ForgetReference(metric *pool.MetricState) {
	metric.ReferenceBase1e8, metric.ReferenceBaseMS = 0, 0
	metric.ReferenceQuote1e8, metric.ReferenceQuoteMS = 0, 0
	metric.BidX64, metric.AskX64 = nil, nil
}

var (
	referencePpm     = big.NewInt(1_000_000)
	referenceBandPpm = big.NewInt(5)
)

// ReferenceBand returns the provider's bounds on the quote, Q64.64 in whole
// tokens: the highest bid and the lowest ask it lets through.
//
//	bid = base * (1e6 - 5) * 2^64 / (quote * 1e6 + 1e6 + 5)
//	ask = (base * (1e6 + 5) + 1e6) * 2^64 / (quote * 1e6)
//
// The fit leaves the provider's last rounding steps open (the bid 2^-62 of
// its value high, the ask up to two units low), so each bound is moved 2^-60
// of its value against the taker: a fill is never worse than quoted.
func ReferenceBand(base1e8, quote1e8 uint64) (bidX64, askX64 *big.Int) {
	if base1e8 == 0 || quote1e8 == 0 {
		return nil, nil
	}
	base, quote := new(big.Int).SetUint64(base1e8), new(big.Int).SetUint64(quote1e8)
	bidX64 = new(big.Int).Mul(base, new(big.Int).Sub(referencePpm, referenceBandPpm))
	denominator := new(big.Int).Mul(quote, referencePpm)
	denominator.Add(denominator, referencePpm).Add(denominator, referenceBandPpm)
	bidX64.Lsh(bidX64, 64).Quo(bidX64, denominator)
	bidX64.Sub(bidX64, new(big.Int).Rsh(bidX64, 60)).Sub(bidX64, bigOne)

	askX64 = new(big.Int).Mul(base, new(big.Int).Add(referencePpm, referenceBandPpm))
	askX64.Add(askX64, referencePpm).Lsh(askX64, 64)
	denominator = new(big.Int).Mul(quote, referencePpm)
	askX64.Add(askX64, denominator).Sub(askX64, bigOne).Quo(askX64, denominator)
	askX64.Add(askX64, new(big.Int).Rsh(askX64, 60)).Add(askX64, bigOne)
	return bidX64, askX64
}

// clampToReference narrows the extension's quote to the provider's band. A
// pool registered with a reference whose feeds are unknown has no quote.
func clampToReference(metric *pool.MetricState) {
	if metric.Reference == (domain.Address{}) || metric.BidX64 == nil {
		return
	}
	bid, ask := ReferenceBand(metric.ReferenceBase1e8, metric.ReferenceQuote1e8)
	if bid == nil {
		metric.BidX64, metric.AskX64 = nil, nil
		return
	}
	if bid.Cmp(metric.BidX64) < 0 {
		metric.BidX64 = bid
	}
	if ask.Cmp(metric.AskX64) > 0 {
		metric.AskX64 = ask
	}
}
