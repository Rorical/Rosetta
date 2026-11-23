package metricomm

import (
	"fmt"

	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Storage layout of MetricOmmPool, as fixed by the contract and mirrored by
// its PoolStateLibrary.
const (
	// slot0Index packs pauseLevel(uint8,0) | curBinIdx(int16,8) |
	// curPosInBin(uint104,24) | curBinDistFromProvidedPriceE6(int24,128) |
	// spreadFeeE6(uint24,152) | notionalFeeE8(uint24,176) |
	// bidAskRefAsymmetryX8(uint16,200).
	slot0Index = 0
	// binTotalsIndex packs scaledToken0(uint128,0) | scaledToken1(uint128,128).
	binTotalsIndex = 1
	// binStatesMappingIndex is the base slot of mapping(int256 => BinState).
	// A BinState packs token0BalanceScaled(uint104,0) |
	// token1BalanceScaled(uint104,104) | lengthE6(uint16,208) |
	// addFeeBuyE6(uint16,224) | addFeeSellE6(uint16,240).
	binStatesMappingIndex = 4
)

// Immutables are the constructor-set values the swap math reads. They live in
// the pool's bytecode, so they come from getImmutables(), not from storage.
type Immutables struct {
	// Token0ScaleMultiplier and Token1ScaleMultiplier convert native token
	// units to the pool's internal units (at least 18 decimals).
	Token0ScaleMultiplier uint256.Int
	Token1ScaleMultiplier uint256.Int
	// LowestBin and HighestBin bound the bin grid, both inclusive.
	LowestBin  int16
	HighestBin int16
}

// Bin is one BinState word.
type Bin struct {
	Token0BalanceScaled uint256.Int // uint104
	Token1BalanceScaled uint256.Int // uint104
	// LengthE6 is the bin's width as a distance from the reference price
	// (1e6 = 100% of the reference).
	LengthE6 uint16
	// AddFeeBuyE6 is charged on top of the base fee when token0 is bought
	// from the bin, AddFeeSellE6 when token0 is sold into it.
	AddFeeBuyE6  uint16
	AddFeeSellE6 uint16
}

// State is everything the swap math reads from a pool.
type State struct {
	Immutables

	// Slot 0.
	PauseLevel uint8
	CurBinIdx  int16
	// CurPosInBin is the position inside the current bin: 0 is the lower
	// edge, 2^104-1 the upper edge.
	CurPosInBin uint256.Int
	// CurBinDistFromProvidedPriceE6 is the lower edge of the current bin
	// relative to the reference price (int24 on chain).
	CurBinDistFromProvidedPriceE6 int32
	SpreadFeeE6                   uint32 // uint24
	NotionalFeeE8                 uint32 // uint24
	// BidAskRefAsymmetryX8 bounds how lopsided bid and ask may sit around the
	// reference; 0xffff disables the check.
	BidAskRefAsymmetryX8 uint16

	// Slot 1: the sums of the bin balances. Exact-output swaps are capped
	// by the output-side total.
	TotalToken0Scaled uint256.Int // uint128
	TotalToken1Scaled uint256.Int // uint128

	// Bins holds every bin from LowestBin to HighestBin; Bins[i] is bin
	// LowestBin+i.
	Bins []Bin
}

// Clone returns a copy that shares no memory with the receiver.
func (s State) Clone() State {
	s.Bins = append([]Bin(nil), s.Bins...)
	return s
}

func (s *State) validate() error {
	want := int(s.HighestBin) - int(s.LowestBin) + 1
	if want <= 0 || len(s.Bins) != want {
		return fmt.Errorf("%w: %d bins for grid %d..%d", ErrInvalidState, len(s.Bins), s.LowestBin, s.HighestBin)
	}
	if s.CurBinIdx < s.LowestBin || s.CurBinIdx > s.HighestBin {
		return fmt.Errorf("%w: current bin %d outside grid %d..%d", ErrInvalidState, s.CurBinIdx, s.LowestBin, s.HighestBin)
	}
	return nil
}

// BinStateSlot returns the storage slot of a bin's BinState word:
// keccak256(abi.encode(int256(binIdx), 4)).
func BinStateSlot(binIdx int16) domain.Hash {
	var preimage [64]byte
	if binIdx < 0 {
		for i := 0; i < 30; i++ {
			preimage[i] = 0xff
		}
	}
	preimage[30] = byte(uint16(binIdx) >> 8)
	preimage[31] = byte(uint16(binIdx))
	preimage[63] = binStatesMappingIndex
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write(preimage[:])
	var slot domain.Hash
	hasher.Sum(slot[:0])
	return slot
}

// StorageSlots lists the slots a full state read needs, in the order
// DecodeState expects: slot 0, slot 1, then one slot per bin from LowestBin
// to HighestBin.
func StorageSlots(immutables Immutables) []domain.Hash {
	slots := make([]domain.Hash, 0, 2+int(immutables.HighestBin)-int(immutables.LowestBin)+1)
	slots = append(slots, domain.Hash{31: slot0Index}, domain.Hash{31: binTotalsIndex})
	for binIdx := int(immutables.LowestBin); binIdx <= int(immutables.HighestBin); binIdx++ {
		slots = append(slots, BinStateSlot(int16(binIdx)))
	}
	return slots
}

// DecodeState builds a State from the storage words read at StorageSlots, in
// the same order.
func DecodeState(immutables Immutables, words []domain.Hash) (State, error) {
	want := 2 + int(immutables.HighestBin) - int(immutables.LowestBin) + 1
	if want <= 2 || len(words) != want {
		return State{}, fmt.Errorf("%w: %d storage words, want %d for grid %d..%d",
			ErrInvalidState, len(words), want, immutables.LowestBin, immutables.HighestBin)
	}
	state := State{Immutables: immutables, Bins: make([]Bin, want-2)}
	state.ApplySlot0(words[0])
	state.ApplyBinTotals(words[1])
	for i, binWord := range words[2:] {
		state.Bins[i] = DecodeBin(binWord)
	}
	if err := state.validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

// ApplySlot0 overwrites the slot 0 fields from the raw storage word.
func (s *State) ApplySlot0(raw domain.Hash) {
	packed := new(word).SetBytes32(raw[:])
	field := func(shift, bits uint) *word {
		mask := new(word).SubUint64(new(word).Lsh(one, bits), 1)
		return mask.And(mask, new(word).Rsh(packed, shift))
	}
	s.PauseLevel = uint8(field(0, 8).Uint64())
	s.CurBinIdx = int16(uint16(field(8, 16).Uint64()))
	s.CurPosInBin = *field(24, 104)
	// Sign-extend the int24 through the top byte of an int32.
	s.CurBinDistFromProvidedPriceE6 = int32(uint32(field(128, 24).Uint64())<<8) >> 8
	s.SpreadFeeE6 = uint32(field(152, 24).Uint64())
	s.NotionalFeeE8 = uint32(field(176, 24).Uint64())
	s.BidAskRefAsymmetryX8 = uint16(field(200, 16).Uint64())
}

// ApplyBinTotals overwrites the slot 1 fields from the raw storage word.
func (s *State) ApplyBinTotals(raw domain.Hash) {
	s.TotalToken0Scaled.SetBytes(raw[16:])
	s.TotalToken1Scaled.SetBytes(raw[:16])
}

// DecodeBin decodes one BinState storage word.
func DecodeBin(raw domain.Hash) Bin {
	var bin Bin
	bin.Token0BalanceScaled.SetBytes(raw[19:32])
	bin.Token1BalanceScaled.SetBytes(raw[6:19])
	bin.LengthE6 = uint16(raw[4])<<8 | uint16(raw[5])
	bin.AddFeeBuyE6 = uint16(raw[2])<<8 | uint16(raw[3])
	bin.AddFeeSellE6 = uint16(raw[0])<<8 | uint16(raw[1])
	return bin
}
