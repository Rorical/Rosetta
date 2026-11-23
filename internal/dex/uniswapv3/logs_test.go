package uniswapv3

import (
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestTopicConstants(t *testing.T) {
	tests := []struct {
		name string
		got  domain.Hash
		want string
	}{
		{"initialize", TopicInitialize, "0x98636036cb66a9c19a37435efc1e90142190214e8abeb821bdba3f2990dd4c95"},
		{"mint", TopicMint, "0x7a53080ba414158be7ec69b987b5fb7d07dee101fe85488f0853ae16239d0bde"},
		{"burn", TopicBurn, "0x0c396cd989a39f4459b5fa1aed6a9a8dcdbc45908acfd67e028cd568da98982c"},
		{"swap", TopicSwap, "0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67"},
		{"collect", TopicCollect, "0x70935338e69775456a85ddef226c395fb668b63fa0115f5f20610b388e6ca9c0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got.Hex() != test.want {
				t.Fatalf("topic = %s, want %s", test.got, test.want)
			}
		})
	}
}

func TestDecodeLogInitialize(t *testing.T) {
	ev, ok, err := DecodeLog(
		[]domain.Hash{TopicInitialize},
		words(big.NewInt(12345), big.NewInt(-120)),
	)
	if err != nil {
		t.Fatalf("DecodeLog: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if ev.Kind != EventInitialize {
		t.Fatalf("kind = %d, want EventInitialize", ev.Kind)
	}
	assertBig(t, "sqrt price", ev.SqrtPriceX96, 12345)
	if ev.Tick != -120 {
		t.Fatalf("tick = %d, want -120", ev.Tick)
	}
}

func TestDecodeLogMintSkipsSenderWord(t *testing.T) {
	sender := big.NewInt(0x1111)
	ev, ok, err := DecodeLog(
		[]domain.Hash{TopicMint, addressTopic(0xaa), int24Topic(-60), int24Topic(60)},
		words(sender, big.NewInt(1000), big.NewInt(7), big.NewInt(9)),
	)
	if err != nil {
		t.Fatalf("DecodeLog: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if ev.Kind != EventMint {
		t.Fatalf("kind = %d, want EventMint", ev.Kind)
	}
	if ev.TickLower != -60 || ev.TickUpper != 60 {
		t.Fatalf("ticks = %d..%d, want -60..60", ev.TickLower, ev.TickUpper)
	}
	// Amount is data word 1. Reading word 0 would yield the sender instead.
	assertBig(t, "amount", ev.Amount, 1000)
}

func TestDecodeLogBurn(t *testing.T) {
	ev, ok, err := DecodeLog(
		[]domain.Hash{TopicBurn, addressTopic(0xbb), int24Topic(-120), int24Topic(120)},
		words(big.NewInt(400), big.NewInt(10), big.NewInt(20)),
	)
	if err != nil {
		t.Fatalf("DecodeLog: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if ev.Kind != EventBurn {
		t.Fatalf("kind = %d, want EventBurn", ev.Kind)
	}
	if ev.TickLower != -120 || ev.TickUpper != 120 {
		t.Fatalf("ticks = %d..%d, want -120..120", ev.TickLower, ev.TickUpper)
	}
	assertBig(t, "amount", ev.Amount, 400)
}

func TestDecodeLogSwap(t *testing.T) {
	ev, ok, err := DecodeLog(
		[]domain.Hash{TopicSwap, addressTopic(0xcc), addressTopic(0xdd)},
		words(big.NewInt(-5), big.NewInt(6), big.NewInt(99), big.NewInt(77), big.NewInt(-42)),
	)
	if err != nil {
		t.Fatalf("DecodeLog: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if ev.Kind != EventSwap {
		t.Fatalf("kind = %d, want EventSwap", ev.Kind)
	}
	assertBig(t, "sqrt price", ev.SqrtPriceX96, 99)
	assertBig(t, "liquidity", ev.Liquidity, 77)
	if ev.Tick != -42 {
		t.Fatalf("tick = %d, want -42", ev.Tick)
	}
}

func TestDecodeLogCollectSkipsData(t *testing.T) {
	ev, ok, err := DecodeLog(
		[]domain.Hash{TopicCollect, addressTopic(0xaa), int24Topic(-60), int24Topic(60)},
		[]byte{0xde, 0xad, 0xbe, 0xef},
	)
	if err != nil {
		t.Fatalf("DecodeLog: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if ev.Kind != EventCollect {
		t.Fatalf("kind = %d, want EventCollect", ev.Kind)
	}
}

func TestDecodeLogIgnoresUnknownTopic(t *testing.T) {
	tests := []struct {
		name   string
		topics []domain.Hash
	}{
		{"no topics", nil},
		{"other event", []domain.Hash{{0xde, 0xad}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ev, ok, err := DecodeLog(test.topics, []byte{0x01})
			if err != nil {
				t.Fatalf("DecodeLog: %v", err)
			}
			if ok {
				t.Fatalf("ok = true, want false")
			}
			if ev.Kind != 0 {
				t.Fatalf("kind = %d, want zero", ev.Kind)
			}
		})
	}
}

func TestDecodeLogMalformed(t *testing.T) {
	tests := []struct {
		name   string
		topics []domain.Hash
		data   []byte
	}{
		{
			name:   "initialize extra topic",
			topics: []domain.Hash{TopicInitialize, addressTopic(0xaa)},
			data:   words(big.NewInt(1), big.NewInt(0)),
		},
		{
			name:   "initialize short data",
			topics: []domain.Hash{TopicInitialize},
			data:   make([]byte, 63),
		},
		{
			name:   "initialize long data",
			topics: []domain.Hash{TopicInitialize},
			data:   make([]byte, 96),
		},
		{
			name:   "initialize tick outside int32",
			topics: []domain.Hash{TopicInitialize},
			data:   words(big.NewInt(1), big.NewInt(1<<31)),
		},
		{
			name:   "mint missing topic",
			topics: []domain.Hash{TopicMint, addressTopic(0xaa), int24Topic(0)},
			data:   words(big.NewInt(0), big.NewInt(1), big.NewInt(0), big.NewInt(0)),
		},
		{
			name:   "mint three data words",
			topics: []domain.Hash{TopicMint, addressTopic(0xaa), int24Topic(0), int24Topic(60)},
			data:   words(big.NewInt(1), big.NewInt(0), big.NewInt(0)),
		},
		{
			name:   "mint five data words",
			topics: []domain.Hash{TopicMint, addressTopic(0xaa), int24Topic(0), int24Topic(60)},
			data:   words(big.NewInt(0), big.NewInt(1), big.NewInt(0), big.NewInt(0), big.NewInt(0)),
		},
		{
			name:   "mint tick outside int32",
			topics: []domain.Hash{TopicMint, addressTopic(0xaa), int24Topic(0), hashWord(big.NewInt(1 << 40))},
			data:   words(big.NewInt(0), big.NewInt(1), big.NewInt(0), big.NewInt(0)),
		},
		{
			name:   "burn missing topic",
			topics: []domain.Hash{TopicBurn, addressTopic(0xaa), int24Topic(0)},
			data:   words(big.NewInt(1), big.NewInt(0), big.NewInt(0)),
		},
		{
			name:   "burn short data",
			topics: []domain.Hash{TopicBurn, addressTopic(0xaa), int24Topic(0), int24Topic(60)},
			data:   words(big.NewInt(1), big.NewInt(0)),
		},
		{
			name:   "swap missing topic",
			topics: []domain.Hash{TopicSwap, addressTopic(0xaa)},
			data:   words(big.NewInt(0), big.NewInt(0), big.NewInt(1), big.NewInt(1), big.NewInt(0)),
		},
		{
			name:   "swap short data",
			topics: []domain.Hash{TopicSwap, addressTopic(0xaa), addressTopic(0xbb)},
			data:   words(big.NewInt(0), big.NewInt(0), big.NewInt(1), big.NewInt(1)),
		},
		{
			name:   "swap tick outside int32",
			topics: []domain.Hash{TopicSwap, addressTopic(0xaa), addressTopic(0xbb)},
			data:   words(big.NewInt(0), big.NewInt(0), big.NewInt(1), big.NewInt(1), big.NewInt(-1<<40)),
		},
		{
			name:   "collect three topics",
			topics: []domain.Hash{TopicCollect, addressTopic(0xaa), int24Topic(0)},
		},
		{
			name:   "collect five topics",
			topics: []domain.Hash{TopicCollect, addressTopic(0xaa), int24Topic(0), int24Topic(60), addressTopic(0xbb)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := DecodeLog(test.topics, test.data); !errors.Is(err, ErrInvalidLog) {
				t.Fatalf("error = %v, want ErrInvalidLog", err)
			}
		})
	}
}

func words(values ...*big.Int) []byte {
	out := make([]byte, 0, len(values)*32)
	for _, value := range values {
		out = append(out, word(value)...)
	}
	return out
}

func word(value *big.Int) []byte {
	encoded := new(big.Int).Set(value)
	if encoded.Sign() < 0 {
		encoded.Add(encoded, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	out := make([]byte, 32)
	encoded.FillBytes(out)
	return out
}

func hashWord(value *big.Int) domain.Hash {
	var hash domain.Hash
	copy(hash[:], word(value))
	return hash
}

func int24Topic(tick int32) domain.Hash {
	return hashWord(big.NewInt(int64(tick)))
}

func addressTopic(b byte) domain.Hash {
	var hash domain.Hash
	hash[31] = b
	return hash
}

func assertBig(t *testing.T, name string, got *big.Int, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %d", name, want)
	}
	if got.Cmp(big.NewInt(want)) != 0 {
		t.Fatalf("%s = %s, want %d", name, got, want)
	}
}

// A Ramses-style fork Mint carries an extra data word before the liquidity
// amount, so the amount is word 2 (not word 1 as in V3), and its topic differs
// from TopicMint.
func TestDecodeLogForkMintReadsAmountAtWordTwo(t *testing.T) {
	if TopicForkMint.Hex() != "0xd78218c0d304e8893cb3200abe394bbc8d5b7804d9c51f236df9fdcf481d02d3" {
		t.Fatalf("fork mint topic = %s", TopicForkMint)
	}
	sender, extra := big.NewInt(0x2222), big.NewInt(29693)
	ev, ok, err := DecodeLog(
		[]domain.Hash{TopicForkMint, addressTopic(0xbb), int24Topic(-199260), int24Topic(-199160)},
		words(sender, extra, big.NewInt(182893894792389), big.NewInt(9614822135769627), big.NewInt(21686885)),
	)
	if err != nil || !ok {
		t.Fatalf("DecodeLog: ok=%v err=%v", ok, err)
	}
	if ev.Kind != EventMint {
		t.Fatalf("kind = %d, want EventMint", ev.Kind)
	}
	if ev.TickLower != -199260 || ev.TickUpper != -199160 {
		t.Fatalf("ticks = %d..%d", ev.TickLower, ev.TickUpper)
	}
	assertBig(t, "amount", ev.Amount, 182893894792389)
	// A V3-shaped (4-word) payload on this topic is malformed, not silently misread.
	if _, _, err := DecodeLog([]domain.Hash{TopicForkMint, addressTopic(0xbb), int24Topic(-60), int24Topic(60)}, words(big.NewInt(1), big.NewInt(2), big.NewInt(3), big.NewInt(4))); err == nil {
		t.Fatal("4-word fork mint accepted")
	}
}

// Pancake V3 appends protocolFeesToken0/1 to Swap, so its topic differs from
// TopicSwap. Real log: Arbitrum One WETH/USDC 0.05% pool 0xd9e2a1a6…, block
// 0x1f25171e, tx 0x7e870d40…, log index 2. Ignoring it froze the
// pool's price and produced phantom arbitrage against live pools.
func TestDecodeLogPancakeSwap(t *testing.T) {
	topic, _ := domain.ParseHash("0x19b47279256b2a23a1665c810c8d55a1758940ee09377d4f8d26497a3577dc83")
	if TopicPancakeSwap != topic {
		t.Fatalf("pancake swap topic = %s", TopicPancakeSwap)
	}
	party, _ := domain.ParseHash("0x000000000000000000000000e6b82c6eccdfb2d6bc55bb11a61bae5ab2c3f807")
	data, err := hex.DecodeString("000000000000000000000000000000000000000000000000017e504f1bb1d580fffffffffffffffffffffffffffffffffffffffffffffffffffffffff246fc2600000000000000000000000000000000000000000003082a7a17a7cb7c0fab59000000000000000000000000000000000000000000000000009f87b5230024d0fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffcf42e000000000000000000000000000000000000000000000000000010a36723b18f0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	ev, ok, err := DecodeLog([]domain.Hash{TopicPancakeSwap, party, party}, data)
	if err != nil || !ok || ev.Kind != EventSwap {
		t.Fatalf("DecodeLog: kind=%d ok=%v err=%v", ev.Kind, ok, err)
	}
	price, _ := new(big.Int).SetString("3082a7a17a7cb7c0fab59", 16)
	if ev.SqrtPriceX96.Cmp(price) != 0 || ev.Liquidity.Cmp(big.NewInt(0x9f87b5230024d0)) != 0 || ev.Tick != -199634 {
		t.Fatalf("event = %+v", ev)
	}
	// A V3-shaped (5-word) payload on this topic is malformed, not misread.
	if _, _, err := DecodeLog([]domain.Hash{TopicPancakeSwap, party, party}, data[:5*32]); err == nil {
		t.Fatal("5-word pancake swap accepted")
	}
}
