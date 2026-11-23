package metricomm

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"github.com/Rorical/Rosetta/internal/domain"
)

// The fixtures under testdata were recorded from the real pools on an
// Arbitrum One archive node.

type fixtureImmutables struct {
	Token0ScaleMultiplier string `json:"token0_scale_multiplier"`
	Token1ScaleMultiplier string `json:"token1_scale_multiplier"`
	LowestBin             int16  `json:"lowest_bin"`
	HighestBin            int16  `json:"highest_bin"`
}

type fixtureStorage struct {
	Slot0 string `json:"slot0"`
	Slot1 string `json:"slot1"`
	Bins  []struct {
		Index int16  `json:"index"`
		Slot  string `json:"slot"`
		Word  string `json:"word"`
	} `json:"bins"`
}

type fixtureOutcome struct {
	Outcome      string `json:"outcome"`
	Amount0Delta string `json:"amount0_delta"`
	Amount1Delta string `json:"amount1_delta"`
	ErrorName    string `json:"error_name"`
}

type fixtureCase struct {
	Name            string `json:"name"`
	ZeroForOne      bool   `json:"zero_for_one"`
	AmountSpecified string `json:"amount_specified"`
	PriceLimitX64   string `json:"price_limit_x64"`
	BidX64          string `json:"bid_x64"`
	AskX64          string `json:"ask_x64"`
	ReferenceX64    string `json:"reference_x64"`
	fixtureOutcome
	// Stubbed is the same call with every extension accepting, recorded when
	// an extension rejected the real one.
	Stubbed *fixtureOutcome `json:"stubbed"`
}

// liveFixture is one pool at one pinned block with simulateSwapAndRevert
// results on its real state.
type liveFixture struct {
	Pool        string            `json:"pool"`
	BlockNumber uint64            `json:"block_number"`
	Immutables  fixtureImmutables `json:"immutables"`
	Storage     fixtureStorage    `json:"storage"`
	Cases       []fixtureCase     `json:"cases"`
}

// overrideFixture reruns the simulation with storage words replaced through
// an eth_call state override.
type overrideFixture struct {
	Pool       string            `json:"pool"`
	Immutables fixtureImmutables `json:"immutables"`
	Storage    fixtureStorage    `json:"storage"`
	Variants   []struct {
		Name             string            `json:"name"`
		StorageOverrides map[string]string `json:"storage_overrides"`
		Cases            []fixtureCase     `json:"cases"`
	} `json:"variants"`
}

// replayFixture is a real swap with the pool's storage before and after.
type replayFixture struct {
	Pool            string            `json:"pool"`
	TransactionHash string            `json:"transaction_hash"`
	Immutables      fixtureImmutables `json:"immutables"`
	StorageBefore   fixtureStorage    `json:"storage_before"`
	StorageAfter    fixtureStorage    `json:"storage_after"`
	ZeroForOne      bool              `json:"zero_for_one"`
	AmountSpecified string            `json:"amount_specified"`
	PriceLimitX64   string            `json:"price_limit_x64"`
	BidX64          string            `json:"bid_x64"`
	AskX64          string            `json:"ask_x64"`
	ReferenceX64    string            `json:"reference_x64"`
	Amount0Delta    string            `json:"amount0_delta"`
	Amount1Delta    string            `json:"amount1_delta"`
}

// poolErrors maps the contract's custom errors to the package's errors.
var poolErrors = map[string]error{
	"InvalidAmount()":                         ErrInvalidAmount,
	"FinalAmountIsZero()":                     ErrFinalAmountIsZero,
	"AmountScaledExceedsMax()":                ErrAmountScaledExceedsMax,
	"BidGreaterThanAsk()":                     ErrBidGreaterThanAsk,
	"BidIsZero()":                             ErrBidIsZero,
	"ReferencePriceOutOfRange()":              ErrReferencePriceOutOfRange,
	"BidAskRefAsymmetryOutOfBounds()":         ErrBidAskRefAsymmetryOutOfBounds,
	"OverflowedOnFittingBinBalances(uint256)": ErrBinBalanceOverflow,
}

// loadFixtures decodes every testdata file whose name match accepts.
func loadFixtures[T any](t *testing.T, match func(name string) bool) []T {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "0x*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []T
	for _, path := range paths {
		if !match(filepath.Base(path)) {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fixture T
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		fixtures = append(fixtures, fixture)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures")
	}
	return fixtures
}

func mustHash(t *testing.T, s string) domain.Hash {
	t.Helper()
	hash, err := domain.ParseHash(s)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	value, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("not a decimal integer: %q", s)
	}
	return value
}

// decodeFixtureState decodes recorded storage through StorageSlots and
// DecodeState, after replacing the words named in overrides.
func decodeFixtureState(t *testing.T, im fixtureImmutables, storage fixtureStorage, overrides map[string]string) State {
	t.Helper()
	immutables := Immutables{
		Token0ScaleMultiplier: *uint256.MustFromDecimal(im.Token0ScaleMultiplier),
		Token1ScaleMultiplier: *uint256.MustFromDecimal(im.Token1ScaleMultiplier),
		LowestBin:             im.LowestBin,
		HighestBin:            im.HighestBin,
	}
	slots := StorageSlots(immutables)
	if len(slots) != 2+len(storage.Bins) {
		t.Fatalf("StorageSlots returned %d slots for %d bins", len(slots), len(storage.Bins))
	}
	words := []domain.Hash{mustHash(t, storage.Slot0), mustHash(t, storage.Slot1)}
	for i, bin := range storage.Bins {
		if slots[2+i] != mustHash(t, bin.Slot) {
			t.Fatalf("bin %d: slot %s, chain read used %s", bin.Index, slots[2+i].Hex(), bin.Slot)
		}
		words = append(words, mustHash(t, bin.Word))
	}
	applied := 0
	for i, slot := range slots {
		if replacement, ok := overrides[slot.Hex()]; ok {
			words[i] = mustHash(t, replacement)
			applied++
		}
	}
	if applied != len(overrides) {
		t.Fatalf("%d of %d storage overrides name a slot outside the state", len(overrides)-applied, len(overrides))
	}
	state, err := DecodeState(immutables, words)
	if err != nil {
		t.Fatal(err)
	}
	if sum0, sum1 := binSums(state); sum0 != state.TotalToken0Scaled || sum1 != state.TotalToken1Scaled {
		t.Fatal("bin totals do not match the bins")
	}
	return state
}

func binSums(state State) (token0, token1 uint256.Int) {
	for i := range state.Bins {
		token0.Add(&token0, &state.Bins[i].Token0BalanceScaled)
		token1.Add(&token1, &state.Bins[i].Token1BalanceScaled)
	}
	return token0, token1
}

type tally struct {
	matched, reverted, skipped int
}

// compareCases runs every case against state and requires the exact chain
// deltas, or the matching revert.
func compareCases(t *testing.T, state State, cases []fixtureCase) tally {
	t.Helper()
	var result tally
	pristine := state.Clone()
	for _, c := range cases {
		want := c.fixtureOutcome
		if want.Outcome != "ok" && want.Outcome != "pool_error" {
			// An extension stopped the real call; compare against the stubbed rerun.
			if c.Stubbed == nil || (c.Stubbed.Outcome != "ok" && c.Stubbed.Outcome != "pool_error") {
				result.skipped++
				continue
			}
			want = *c.Stubbed
		}
		amount0, amount1, next, err := Swap(state, c.ZeroForOne, mustBig(t, c.AmountSpecified),
			mustBig(t, c.PriceLimitX64), mustBig(t, c.BidX64), mustBig(t, c.AskX64), mustBig(t, c.ReferenceX64))
		if !reflect.DeepEqual(state, pristine) {
			t.Fatalf("%s: Swap mutated its input state", c.Name)
		}
		if want.Outcome == "pool_error" {
			if err == nil {
				t.Errorf("%s: chain reverted with %s, Swap returned (%s, %s)", c.Name, want.ErrorName, amount0, amount1)
				continue
			}
			if wantErr, known := poolErrors[want.ErrorName]; known && !errors.Is(err, wantErr) {
				t.Errorf("%s: chain reverted with %s, Swap returned %v", c.Name, want.ErrorName, err)
				continue
			}
			result.reverted++
			continue
		}
		if err != nil {
			t.Errorf("%s: chain returned (%s, %s), Swap failed: %v", c.Name, want.Amount0Delta, want.Amount1Delta, err)
			continue
		}
		if amount0.String() != want.Amount0Delta || amount1.String() != want.Amount1Delta {
			t.Errorf("%s: deltas (%s, %s), chain (%s, %s)", c.Name, amount0, amount1, want.Amount0Delta, want.Amount1Delta)
			continue
		}
		if sum0, sum1 := binSums(next); sum0 != next.TotalToken0Scaled || sum1 != next.TotalToken1Scaled {
			t.Errorf("%s: next state bin totals do not match its bins", c.Name)
		}
		result.matched++
	}
	return result
}

// isLive selects the live fixtures: the files named after the pool alone.
func isLive(name string) bool {
	return !strings.Contains(name, "-")
}

func TestSwapMatchesChain(t *testing.T) {
	for _, f := range loadFixtures[liveFixture](t, isLive) {
		t.Run(f.Pool, func(t *testing.T) {
			state := decodeFixtureState(t, f.Immutables, f.Storage, nil)
			if state.PauseLevel != 0 {
				t.Fatalf("fixture pool is paused (level %d); simulateSwapAndRevert ignores pause, Swap does not", state.PauseLevel)
			}
			result := compareCases(t, state, f.Cases)
			t.Logf("block %d: %d cases, %d exact matches, %d matching reverts, %d skipped (extension)",
				f.BlockNumber, len(f.Cases), result.matched, result.reverted, result.skipped)
			if result.matched == 0 || result.reverted == 0 {
				t.Fatalf("fixture exercised %d successes and %d reverts", result.matched, result.reverted)
			}
		})
	}
}

// TestSwapMatchesChainWithStorageOverrides covers what the live pools leave
// idle (notional fee, per-bin fees, the asymmetry bound, drained bins inside
// the range): the chain ran the real bytecode over edited storage words.
func TestSwapMatchesChainWithStorageOverrides(t *testing.T) {
	isOverrides := func(name string) bool { return strings.HasSuffix(name, "-overrides.json") }
	for _, f := range loadFixtures[overrideFixture](t, isOverrides) {
		for _, variant := range f.Variants {
			t.Run(f.Pool+"/"+variant.Name, func(t *testing.T) {
				state := decodeFixtureState(t, f.Immutables, f.Storage, variant.StorageOverrides)
				result := compareCases(t, state, variant.Cases)
				t.Logf("%d cases, %d exact matches, %d matching reverts", len(variant.Cases), result.matched, result.reverted)
				if result.matched+result.reverted != len(variant.Cases) {
					t.Fatalf("%d of %d cases compared", result.matched+result.reverted, len(variant.Cases))
				}
			})
		}
	}
}

// TestSwapReplaysRealSwaps checks the state Swap returns: for swaps that
// landed on chain it must equal the pool's storage after the block.
func TestSwapReplaysRealSwaps(t *testing.T) {
	isReplay := func(name string) bool { return strings.Contains(name, "-replay-") }
	for _, f := range loadFixtures[replayFixture](t, isReplay) {
		t.Run(f.Pool+"/"+f.TransactionHash[:10], func(t *testing.T) {
			before := decodeFixtureState(t, f.Immutables, f.StorageBefore, nil)
			after := decodeFixtureState(t, f.Immutables, f.StorageAfter, nil)
			amount0, amount1, next, err := Swap(before, f.ZeroForOne, mustBig(t, f.AmountSpecified),
				mustBig(t, f.PriceLimitX64), mustBig(t, f.BidX64), mustBig(t, f.AskX64), mustBig(t, f.ReferenceX64))
			if err != nil {
				t.Fatal(err)
			}
			if amount0.String() != f.Amount0Delta || amount1.String() != f.Amount1Delta {
				t.Fatalf("deltas (%s, %s), chain (%s, %s)", amount0, amount1, f.Amount0Delta, f.Amount1Delta)
			}
			if !reflect.DeepEqual(next, after) {
				t.Fatalf("next state differs from chain storage after the swap: bin %d pos %s dist %d, chain bin %d pos %s dist %d",
					next.CurBinIdx, &next.CurPosInBin, next.CurBinDistFromProvidedPriceE6,
					after.CurBinIdx, &after.CurPosInBin, after.CurBinDistFromProvidedPriceE6)
			}
			t.Logf("bins crossed: %d", int(after.CurBinIdx)-int(before.CurBinIdx))
		})
	}
}

func TestSwapRejectsBeforeTheMath(t *testing.T) {
	live := loadFixtures[liveFixture](t, isLive)[0]
	state := decodeFixtureState(t, live.Immutables, live.Storage, nil)
	c := live.Cases[0]
	amount, limit := mustBig(t, c.AmountSpecified), mustBig(t, c.PriceLimitX64)
	bid, ask, reference := mustBig(t, c.BidX64), mustBig(t, c.AskX64), mustBig(t, c.ReferenceX64)
	paused := state.Clone()
	paused.PauseLevel = 1
	truncated := state.Clone()
	truncated.Bins = truncated.Bins[1:]
	tooWide := new(big.Int).Lsh(big.NewInt(1), 128)

	tests := []struct {
		name      string
		state     State
		amount    *big.Int
		reference *big.Int
		want      error
	}{
		{"paused pool", paused, amount, reference, ErrPoolPaused},
		{"bin grid shorter than the immutables", truncated, amount, reference, ErrInvalidState},
		{"nil amount", state, nil, reference, ErrInvalidInput},
		{"amount outside int128", state, tooWide, reference, ErrInvalidInput},
		{"reference outside uint128", state, amount, tooWide, ErrInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, _, err := Swap(test.state, c.ZeroForOne, test.amount, limit, bid, ask, test.reference)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}
