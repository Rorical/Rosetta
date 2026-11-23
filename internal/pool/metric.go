package pool

import (
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// MetricState is the raw state of a MetricOmmPool. The swap math lives in
// internal/dex/metricomm, which this package cannot import, so the storage
// words stay undecoded here.
type MetricState struct {
	// Token0Scale and Token1Scale convert native token units to the pool's
	// internal units. They are immutables of the pool.
	Token0Scale *big.Int
	Token1Scale *big.Int
	// LowestBin and HighestBin bound the bin grid, both inclusive.
	LowestBin  int16
	HighestBin int16
	// Words are the pool's storage words in the order of
	// metricomm.StorageSlots: slot 0, the bin totals, then one word per bin
	// from LowestBin to HighestBin.
	Words []domain.Hash

	// Oracle is the contract whose update transactions carry this pool's
	// price, and FeedIndex is the pool's feed inside one update.
	Oracle    domain.Address
	FeedIndex uint8
	// OracleSlot is the oracle's storage slot that holds the last update:
	// the quote can be read from it at a block without the update's calldata.
	OracleSlot domain.Hash
	// QuoteExtension is the pool's quote extension and QuoteExtensionSlot the
	// first slot of its record for this pool; SkewConfig, SkewParameters and
	// SkewState are read from that record (metricomm.SkewSlots). The record's
	// slot was read off a trace, so it is configuration, not derived.
	QuoteExtension     domain.Address
	QuoteExtensionSlot domain.Hash
	// Reference is the oracle the pool's price provider checks the quote
	// against: the price of ReferenceBaseFeed over that of ReferenceQuoteFeed
	// (metricomm.ReferenceBand). The zero address leaves the quote unchecked.
	Reference          domain.Address
	ReferenceBaseFeed  uint32
	ReferenceQuoteFeed uint32
	// ReferenceBase1e8 and ReferenceQuote1e8 are the two feeds' last prices,
	// zero while unknown, and ReferenceBaseMS and ReferenceQuoteMS the
	// timestamps their updates carry.
	ReferenceBase1e8  uint64
	ReferenceQuote1e8 uint64
	ReferenceBaseMS   uint64
	ReferenceQuoteMS  uint64
	// BidX64 and AskX64 are the current quote, Q64.64 token1 per token0 in
	// whole tokens. Both are nil while no valid quote is known.
	BidX64 *big.Int
	AskX64 *big.Int
	// QuoteMS is the millisecond timestamp the oracle update itself carries,
	// not the time it was observed.
	QuoteMS uint64
	// OracleWord is the oracle's storage word after the last update seen
	// (the update's calldata without its leading byte); zero when unknown. A
	// send is held at it: a newer update reprices the pool.
	OracleWord domain.Hash
	// Price1e8, BidSpreadPpm and AskSpreadPpm are this pool's feed in the
	// last oracle update. The quote is priced from them again when a swap
	// moves the pool, since the extension's widening follows the position.
	Price1e8     uint64
	BidSpreadPpm uint32
	AskSpreadPpm uint32

	// SkewConfig, SkewParameters and SkewState are the quote extension's
	// storage words for this pool (metricomm.DecodeSkewConfig, SkewPpm). The
	// extension widens one side of the quote while the pool sits away from
	// the level it tracks.
	SkewConfig     domain.Hash
	SkewParameters domain.Hash
	SkewState      domain.Hash

	// SkewApprox means the pool was swapped since SkewState was read:
	// SkewState was derived by the extension's rule, not read. It is exact
	// on a block with a header time and within a second's drift of the level
	// on a Fast view; the next read replaces it.
	SkewApprox bool

	// Stale means Words no longer follow the chain: the pool emitted an
	// event the log replay does not model. They must be re-read from storage
	// before the pool is quoted.
	Stale bool
}

// Copy returns a deep copy; a nil receiver stays nil.
func (m *MetricState) Copy() *MetricState {
	if m == nil {
		return nil
	}
	copied := *m
	copied.Token0Scale = cloneInt(m.Token0Scale)
	copied.Token1Scale = cloneInt(m.Token1Scale)
	copied.Words = slices.Clone(m.Words)
	copied.BidX64 = cloneInt(m.BidX64)
	copied.AskX64 = cloneInt(m.AskX64)
	return &copied
}
