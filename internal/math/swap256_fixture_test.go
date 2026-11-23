package math_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/solidlyv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
)

// recordedPool is the eth_call recording of a Solidly V3 pool under
// internal/dex/solidlyv3/testdata; its own test checks quotes against the
// pool's on-chain quoteSwap.
type recordedPool struct {
	Pool        string
	Block       uint64
	Token0      string
	Token1      string
	TickSpacing int32 `json:"tick_spacing"`
	WordRadius  int16 `json:"word_radius"`
	Calls       map[string]string
}

func (r recordedPool) CallContract(_ context.Context, _ domain.Address, data []byte, _ string, out *string) error {
	value, ok := r.Calls["0x"+hex.EncodeToString(data)]
	if !ok {
		return fmt.Errorf("unrecorded call 0x%x", data)
	}
	*out = value
	return nil
}

func loadRecordedV3States(t *testing.T) []math.V3State {
	t.Helper()
	files, err := filepath.Glob("../dex/solidlyv3/testdata/op-*.json")
	if err != nil || len(files) == 0 {
		t.Fatal("no recorded pools", err)
	}
	states := make([]math.V3State, 0, len(files))
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var r recordedPool
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		address := func(text string) domain.Address {
			a, err := domain.ParseAddress(text)
			if err != nil {
				t.Fatal(err)
			}
			return a
		}
		spec := solidlyv3.Spec{Address: address(r.Pool), Token0: address(r.Token0), Token1: address(r.Token1), TickSpacing: r.TickSpacing}
		state, deep, err := solidlyv3.Load(t.Context(), r, spec, fmt.Sprintf("0x%x", r.Block), r.WordRadius)
		if err != nil {
			t.Fatal(file, err)
		}
		states = append(states, math.V3State{
			SqrtPriceX96: state.SqrtPriceX96, Tick: state.Tick, Liquidity: state.Liquidity,
			FeePips: state.Fee, TickSpacing: state.TickSpacing,
			Bitmap: deep.Bitmap, Ticks: deep.Ticks, WindowMin: deep.WindowMin, WindowMax: deep.WindowMax,
		})
	}
	return states
}

// Recorded on-chain states, each also with its window removed so swaps walk
// past the loaded ticks, across amounts from 1 wei to 2^200 and with price
// limits, must give the arbitrary-precision result exactly.
func TestSwapV3ExactInMatchesBigOnRecordedPools(t *testing.T) {
	random := rand.New(rand.NewSource(159011751))
	cases, crossings := 0, 0
	for _, recorded := range loadRecordedV3States(t) {
		unbounded := recorded
		unbounded.WindowMin, unbounded.WindowMax = 0, 0
		for _, state := range []math.V3State{recorded, unbounded} {
			for range 60 {
				zeroForOne := random.Intn(2) == 0
				amount := new(big.Int).Lsh(big.NewInt(1+random.Int63n(1<<40)), uint(random.Intn(161)))
				var limit *big.Int
				if random.Intn(3) == 0 {
					offset := int32(1 + random.Intn(2000))
					limitTick := state.Tick - offset
					if !zeroForOne {
						limitTick = state.Tick + offset
					}
					var err error
					if limit, err = math.TickToSqrtPriceX96(limitTick); err != nil {
						t.Fatal(err)
					}
				}
				want, wantErr := math.SwapV3ExactInBig(state, zeroForOne, amount, limit)
				got, err := math.SwapV3ExactInWithLimit(state, zeroForOne, amount, limit)
				if fmt.Sprint(err) != fmt.Sprint(wantErr) {
					t.Fatalf("tick %d zeroForOne=%v amount %s: error %v, want %v", state.Tick, zeroForOne, amount, err, wantErr)
				}
				cases++
				if err != nil {
					continue
				}
				crossings += len(got.Crossed)
				if got.AmountOut.Cmp(want.AmountOut) != 0 || got.AmountInUsed.Cmp(want.AmountInUsed) != 0 ||
					got.SqrtPriceX96.Cmp(want.SqrtPriceX96) != 0 || got.Liquidity.Cmp(want.Liquidity) != 0 ||
					got.Tick != want.Tick || got.HitLimit != want.HitLimit || !slices.Equal(got.Crossed, want.Crossed) {
					t.Fatalf("tick %d zeroForOne=%v amount %s limit %v:\n got %+v\nwant %+v", state.Tick, zeroForOne, amount, limit, got, want)
				}
			}
		}
	}
	if crossings == 0 {
		t.Fatal("no recorded swap crossed an initialized tick")
	}
	t.Logf("%d cases, %d tick crossings", cases, crossings)
}
