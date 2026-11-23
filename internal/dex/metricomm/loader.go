package metricomm

import (
	"bytes"
	"fmt"
	"math/big"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// immutablesWords is the length of getImmutables()' answer in words. The
// layout was read off the verified pool source and is the one the fixture
// script decodes: word 1 token0, 2 token1, 3 and 4 the scale multipliers,
// 9 and 10 the lowest and highest bin, 11 to 17 the extensions.
const immutablesWords = 24

// ImmutablesCall is the calldata of the pool's getImmutables().
func ImmutablesCall() []byte {
	return selector("getImmutables()")
}

// Register fills a pool's constructor-set values from getImmutables()'
// answer and returns its tokens. registration carries the configured oracle
// and quote extension; the answer must list that extension, so a record slot
// configured for another extension cannot be read as this pool's.
func Register(registration pool.MetricState, output []byte) (pool.MetricState, domain.Address, domain.Address, error) {
	if len(output) < immutablesWords*32 {
		return pool.MetricState{}, domain.Address{}, domain.Address{}, fmt.Errorf("%w: getImmutables returned %d bytes", ErrInvalidState, len(output))
	}
	word := func(i int) []byte { return output[i*32 : (i+1)*32] }
	address := func(i int) (domain.Address, error) {
		if !bytes.Equal(word(i)[:12], make([]byte, 12)) {
			return domain.Address{}, fmt.Errorf("%w: immutable %d is not an address", ErrInvalidState, i)
		}
		return domain.Address(word(i)[12:]), nil
	}
	token0, err := address(1)
	if err != nil {
		return pool.MetricState{}, domain.Address{}, domain.Address{}, err
	}
	token1, err := address(2)
	if err != nil {
		return pool.MetricState{}, domain.Address{}, domain.Address{}, err
	}
	bin := func(i int) (int16, error) {
		value := new(big.Int).SetBytes(word(i))
		if value.Bit(255) == 1 {
			value.Sub(value, new(big.Int).Lsh(bigOne, 256))
		}
		if !value.IsInt64() || value.Int64() < -1<<15 || value.Int64() >= 1<<15 {
			return 0, fmt.Errorf("%w: bin bound %s outside int16", ErrInvalidState, value)
		}
		return int16(value.Int64()), nil
	}
	registered := registration
	registered.Token0Scale = new(big.Int).SetBytes(word(3))
	registered.Token1Scale = new(big.Int).SetBytes(word(4))
	if registered.LowestBin, err = bin(9); err != nil {
		return pool.MetricState{}, domain.Address{}, domain.Address{}, err
	}
	if registered.HighestBin, err = bin(10); err != nil {
		return pool.MetricState{}, domain.Address{}, domain.Address{}, err
	}
	if _, err := immutablesOf(&registered); err != nil {
		return pool.MetricState{}, domain.Address{}, domain.Address{}, err
	}
	if registered.HighestBin < registered.LowestBin {
		return pool.MetricState{}, domain.Address{}, domain.Address{}, fmt.Errorf("%w: bin grid %d..%d", ErrInvalidState, registered.LowestBin, registered.HighestBin)
	}
	listed := false
	for i := 11; i <= 17; i++ {
		extension, err := address(i)
		if err != nil {
			return pool.MetricState{}, domain.Address{}, domain.Address{}, err
		}
		listed = listed || extension == registration.QuoteExtension
	}
	if !listed || registration.QuoteExtension == (domain.Address{}) {
		return pool.MetricState{}, domain.Address{}, domain.Address{}, fmt.Errorf("%w: quote extension %s is not one of the pool's extensions", ErrInvalidState, registration.QuoteExtension)
	}
	return *Unloaded(&registered), token0, token1, nil
}

// Unloaded returns the pool's registration without anything read from
// storage: no words, no extension state, no quote, and stale. A pool starts
// from it at every seed and snapshot restore, because its bins move with
// every swap and its quote every two seconds.
func Unloaded(metric *pool.MetricState) *pool.MetricState {
	return &pool.MetricState{
		Token0Scale: new(big.Int).Set(metric.Token0Scale), Token1Scale: new(big.Int).Set(metric.Token1Scale),
		LowestBin: metric.LowestBin, HighestBin: metric.HighestBin,
		Oracle: metric.Oracle, FeedIndex: metric.FeedIndex, OracleSlot: metric.OracleSlot,
		QuoteExtension: metric.QuoteExtension, QuoteExtensionSlot: metric.QuoteExtensionSlot,
		Reference: metric.Reference, ReferenceBaseFeed: metric.ReferenceBaseFeed, ReferenceQuoteFeed: metric.ReferenceQuoteFeed,
		Stale: true,
	}
}

// WordsCall is the calldata of extsload(bytes32[]) for every word of
// StorageSlots, and the number of words the answer must hold.
func WordsCall(metric *pool.MetricState) ([]byte, int, error) {
	immutables, err := immutablesOf(metric)
	if err != nil {
		return nil, 0, err
	}
	slots := StorageSlots(immutables)
	data := make([]byte, 0, 4+64+32*len(slots))
	data = append(data, selector("extsload(bytes32[])")...)
	data = append(data, abiWord(32)...)
	data = append(data, abiWord(uint64(len(slots)))...)
	for _, slot := range slots {
		data = append(data, slot[:]...)
	}
	return data, len(slots), nil
}

// DecodeWords reads the bytes32[] an extsload call returns.
func DecodeWords(output []byte, count int) ([]domain.Hash, error) {
	if len(output) != 64+32*count || !bytes.Equal(output[:32], abiWord(32)) || !bytes.Equal(output[32:64], abiWord(uint64(count))) {
		return nil, fmt.Errorf("%w: extsload answer of %d bytes for %d words", ErrInvalidState, len(output), count)
	}
	words := make([]domain.Hash, count)
	for i := range words {
		copy(words[i][:], output[64+32*i:])
	}
	return words, nil
}

// SkewSlots lists the quote extension's slots for the pool, in the order
// SkewConfig, SkewParameters, SkewState: the first word of the pool's record
// and the words 9 and 10 slots after it.
func SkewSlots(metric *pool.MetricState) [3]domain.Hash {
	base := new(big.Int).SetBytes(metric.QuoteExtensionSlot[:])
	var slots [3]domain.Hash
	for i, offset := range [3]int64{0, 9, 10} {
		new(big.Int).Add(base, big.NewInt(offset)).FillBytes(slots[i][:])
	}
	return slots
}

// Refresh replaces everything the pool reads from storage with one block's
// values and clears Stale: words is the WordsCall answer, skew the words at
// SkewSlots and oracleWord the oracle's word at OracleSlot, all at the block
// whose timestamp is blockTime. Words that do not describe a pool are
// refused and leave metric untouched.
func Refresh(metric *pool.MetricState, words []domain.Hash, skew [3]domain.Hash, oracleWord domain.Hash, blockTime uint64) error {
	immutables, err := immutablesOf(metric)
	if err != nil {
		return err
	}
	if _, err := DecodeState(immutables, words); err != nil {
		return err
	}
	metric.Words = words
	metric.SkewConfig, metric.SkewParameters, metric.SkewState = skew[0], skew[1], skew[2]
	metric.Stale, metric.SkewApprox = false, false
	FollowOracleWord(metric, oracleWord, blockTime)
	return nil
}

// FollowOracleWord sets the quote from the oracle's stored word, which is an
// update's calldata without its leading zero byte. A word that is not an
// update clears the quote.
func FollowOracleWord(metric *pool.MetricState, word domain.Hash, blockTime uint64) {
	update, err := DecodeOracleUpdate(append([]byte{0}, word[:]...))
	if err != nil {
		ForgetQuote(metric)
		return
	}
	FollowOracle(metric, update, blockTime)
}

func selector(signature string) []byte {
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write([]byte(signature))
	return hasher.Sum(nil)[:4]
}

func abiWord(value uint64) []byte {
	word := make([]byte, 32)
	new(big.Int).SetUint64(value).FillBytes(word)
	return word
}
