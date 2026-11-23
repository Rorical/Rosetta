package uniswapv2

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestTopicSyncConstant(t *testing.T) {
	const want = "0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1"
	if got := TopicSync.Hex(); got != want {
		t.Fatalf("TopicSync = %s, want %s", got, want)
	}
}

func TestDecodeSync(t *testing.T) {
	reserve0, reserve1, ok, err := DecodeSync(
		[]domain.Hash{TopicSync},
		syncData(big.NewInt(1_500), big.NewInt(2_700)),
	)
	if err != nil {
		t.Fatalf("DecodeSync: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	assertSyncReserve(t, "reserve0", reserve0, 1_500)
	assertSyncReserve(t, "reserve1", reserve1, 2_700)
}

func TestDecodeSyncLargeReserves(t *testing.T) {
	// uint112 is the on-chain reserve type; max uint112 must decode exactly.
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))
	reserve0, reserve1, ok, err := DecodeSync([]domain.Hash{TopicSync}, syncData(max, max))
	if err != nil {
		t.Fatalf("DecodeSync: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if reserve0.Cmp(max) != 0 || reserve1.Cmp(max) != 0 {
		t.Fatalf("reserves = %s/%s, want %s", reserve0, reserve1, max)
	}
}

func TestDecodeSyncIgnoresOtherEvents(t *testing.T) {
	tests := []struct {
		name   string
		topics []domain.Hash
	}{
		{"no topics", nil},
		{"unknown topic", []domain.Hash{{0xde, 0xad}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, ok, err := DecodeSync(test.topics, []byte{0x01}); err != nil || ok {
				t.Fatalf("ok = %v, err = %v; want false, nil", ok, err)
			}
		})
	}
}

func TestDecodeSyncMalformed(t *testing.T) {
	tests := []struct {
		name   string
		topics []domain.Hash
		data   []byte
	}{
		{"extra topic", []domain.Hash{TopicSync, {0xaa}}, syncData(big.NewInt(1), big.NewInt(2))},
		{"empty data", []domain.Hash{TopicSync}, nil},
		{"one word", []domain.Hash{TopicSync}, make([]byte, 32)},
		{"three words", []domain.Hash{TopicSync}, make([]byte, 96)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, err := DecodeSync(test.topics, test.data); !errors.Is(err, ErrInvalidLog) {
				t.Fatalf("error = %v, want ErrInvalidLog", err)
			}
		})
	}
}

func syncData(reserve0, reserve1 *big.Int) []byte {
	data := make([]byte, 64)
	reserve0.FillBytes(data[:32])
	reserve1.FillBytes(data[32:])
	return data
}

func assertSyncReserve(t *testing.T, name string, got *big.Int, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %d", name, want)
	}
	if got.Cmp(big.NewInt(want)) != 0 {
		t.Fatalf("%s = %s, want %d", name, got, want)
	}
}
