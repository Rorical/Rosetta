package math

import (
	"math/big"
	"testing"
)

func TestBitmapLimbScanMatchesBitReference(t *testing.T) {
	words := []*big.Int{nil, new(big.Int), big.NewInt(-1), big.NewInt(-127), new(big.Int).Set(uint256Max)}
	for bit := 0; bit <= 300; bit++ {
		words = append(words, new(big.Int).SetBit(new(big.Int), bit, 1))
	}
	for _, word := range words {
		var original, masked big.Int
		if word != nil {
			original.Set(word)
			masked.And(word, uint256Max)
		}
		for position := range 256 {
			for _, lte := range []bool{true, false} {
				want, found := 0, false
				step := 1
				if lte {
					step = -1
				}
				for bit := position; bit >= 0 && bit < 256; bit += step {
					if masked.Bit(bit) != 0 {
						want, found = bit, true
						break
					}
				}
				got, ok := bitmapBit(word, position, lte)
				if ok != found || (ok && got != want) {
					t.Fatalf("word %v position %d lte %v: got %d/%v, want %d/%v", word, position, lte, got, ok, want, found)
				}
			}
		}
		if word != nil && original.Cmp(word) != 0 {
			t.Fatal("bitmap scan mutated its input")
		}
	}
}

func BenchmarkTickBitmapScan(b *testing.B) {
	bitmap := map[int16]*big.Int{0: wordWithBits(0, 63, 64, 127, 128, 255)}
	b.ReportAllocs()
	for b.Loop() {
		for position := range int32(256) {
			TickBitmapNextInitialized(bitmap, position, 1, true)
			TickBitmapNextInitialized(bitmap, position, 1, false)
		}
	}
}
