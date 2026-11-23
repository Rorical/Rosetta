package remote

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
)

// sealOffset is block - sequence on the fixture chain.
const sealOffset = 5

func newTestSealer(t *testing.T) (*fastSealer, chan flashSeal) {
	t.Helper()
	arrivals := newFeedArrivals()
	arrivals.observe(domain.Hash{0xee}, 1, time.Now())
	arrivals.learn(domain.Hash{0xee}, 1+sealOffset)
	seals := make(chan flashSeal, 64)
	return newFastSealer(seals, arrivals, 0, 0), seals
}

func standardAt(number, timestamp, l1 uint64) feed.Message {
	return feed.Message{Sequence: number - sealOffset, BlockHash: domain.Hash{byte(number)}, Header: feed.Header{Timestamp: timestamp, L1BlockNumber: l1}}
}

func fastAt(number, index uint64) feed.FastMessage {
	return feed.FastMessage{BlockNumber: number, TxIndex: index, TxHash: domain.Hash{byte(number), byte(index)}, Receipt: feed.FastReceipt{
		Successful: true, GasUsed: 100, EffectiveGasPrice: big.NewInt(12), BaseFee: big.NewInt(10),
		Logs: []feed.FastLog{{Address: domain.Address{byte(index)}, Topics: []domain.Hash{{1}}, Data: []byte{byte(number)}}},
	}}
}

func sealsOf(seals chan flashSeal) []flashSeal {
	var out []flashSeal
	for {
		select {
		case seal := <-seals:
			out = append(out, seal)
		default:
			return out
		}
	}
}

func TestFastSealerAssemblesACompleteBlock(t *testing.T) {
	s, seals := newTestSealer(t)
	s.standard(standardAt(9, 50, 7))
	s.fast(fastAt(10, 0))
	s.fast(fastAt(10, 1))
	// The header goes back in time; ArbOS keeps the parent's values.
	s.standard(standardAt(10, 49, 6))
	if got := sealsOf(seals); len(got) != 1 || got[0].number != 9 || got[0].block != nil {
		t.Fatal("block 9 has no Fast content and no parent message", got)
	}
	if got := sealsOf(seals); len(got) != 0 {
		t.Fatal("sealed 10 before its last transaction was known", got)
	}
	s.fast(fastAt(11, 0))
	got := sealsOf(seals)
	if len(got) != 1 || got[0].block == nil {
		t.Fatal("complete block not sealed", got)
	}
	block := got[0].block
	want := domain.BlockRef{Number: 10, Hash: domain.Hash{byte(10)}, ParentHash: domain.Hash{byte(9)}, Timestamp: 50, L1Block: 7}
	if block.block.Ref != want || block.block.GasLimit != nitroBlockGasLimit || block.block.Coinbase != nitroCoinbase || block.block.BaseFee.Int64() != 10 {
		t.Fatal("wrong block identity", block.block)
	}
	if len(block.transactions) != 2 || block.transactions[1].Index != 1 || block.transactions[1].Hash != (domain.Hash{byte(10), 1}) || len(block.block.Transactions) != 2 {
		t.Fatal("wrong transactions", block.transactions)
	}
	fees := block.transactions[0].Fees
	if fees == nil || fees.Total.Int64() != 1200 || fees.PriorityFee().Int64() != 2 || fees.Family != domain.FamilyNitro {
		t.Fatal("wrong fees", fees)
	}
}

func TestFastSealerHintsWhatItCannotTrust(t *testing.T) {
	for _, tc := range []struct {
		name string
		feed func(s *fastSealer)
	}{
		{"index gap", func(s *fastSealer) {
			s.standard(standardAt(9, 50, 7))
			s.fast(fastAt(10, 0))
			s.fast(fastAt(10, 2))
			s.standard(standardAt(10, 50, 7))
			s.fast(fastAt(11, 0))
		}},
		{"replaced block", func(s *fastSealer) {
			s.standard(standardAt(9, 50, 7))
			s.fast(fastAt(10, 0))
			s.fast(fastAt(10, 0))
			s.standard(standardAt(10, 50, 7))
			s.fast(fastAt(11, 0))
		}},
		{"missing parent message", func(s *fastSealer) {
			s.standard(standardAt(8, 50, 7))
			s.fast(fastAt(10, 0))
			s.standard(standardAt(10, 50, 7))
			s.fast(fastAt(11, 0))
		}},
		{"fast reconnect", func(s *fastSealer) {
			s.standard(standardAt(9, 50, 7))
			s.fast(fastAt(10, 0))
			s.reset()
			s.fast(fastAt(10, 1))
			s.standard(standardAt(10, 50, 7))
			s.fast(fastAt(11, 0))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, seals := newTestSealer(t)
			tc.feed(s)
			var last *flashSeal
			for _, seal := range sealsOf(seals) {
				if seal.number == 10 {
					last = &seal
				}
			}
			if last == nil || last.block != nil || last.hash != (domain.Hash{byte(10)}) {
				t.Fatal("block 10 not sent as a hint", last)
			}
		})
	}
}

// Without Fast data the standard feed still names each block, a second late,
// so the follower can read it by hash.
func TestFastSealerHintsWhenTheFastStreamStops(t *testing.T) {
	s, seals := newTestSealer(t)
	for number := uint64(10); number < 10+fastSealWait; number++ {
		s.standard(standardAt(number, 50, 7))
	}
	if got := sealsOf(seals); len(got) != 0 {
		t.Fatal("did not wait for the Fast stream", got)
	}
	s.standard(standardAt(10+fastSealWait, 50, 7))
	if got := sealsOf(seals); len(got) != 1 || got[0].number != 10 || got[0].block != nil {
		t.Fatal("stalled without the Fast stream", got)
	}
}

// A block built from a delayed inbox message or a batch posting report has
// the message's poster as its coinbase, not the sequencer. On Arbitrum One,
// blocks 523013787 (two internal transactions, coinbase 0xc1b63485…) and
// 523013781 (a retryable, coinbase 0xd297fa91…) differ from RPC in the
// header only; a sealed hash mismatch makes the follower reread every pool
// (tens of thousands of calls, minutes without blocks).
func TestFastSealerTakesTheCoinbaseFromThePoster(t *testing.T) {
	s, seals := newTestSealer(t)
	poster := domain.Address{0xc1, 0xb6, 0x34, 0x85}
	s.standard(standardAt(9, 50, 7))
	report := standardAt(10, 50, 7)
	report.Header.Poster = &poster
	s.fast(fastAt(10, 0))
	s.standard(report)
	s.fast(fastAt(11, 0))
	s.standard(standardAt(11, 50, 7))
	s.fast(fastAt(12, 0))
	got := sealsOf(seals)
	if len(got) != 3 || got[1].block == nil || got[2].block == nil {
		t.Fatalf("seals: %+v", got)
	}
	if got[1].block.block.Coinbase != poster {
		t.Fatalf("coinbase %s, want the poster", got[1].block.block.Coinbase.Hex())
	}
	if got[2].block.block.Coinbase != nitroCoinbase {
		t.Fatal("a sequencer message without a named poster keeps the sequencer coinbase")
	}
}
