package state

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
)

func TestLiquidityLogsPreserveCompleteTickWindow(t *testing.T) {
	for _, bounds := range [][2]int32{{-60, 60}, {-6000, 6000}, {-6000, 60}} {
		store := seededStore(t, testAddress(0x61))
		key := v3Key(testAddress(0x61))
		deep, _ := store.Deep(key)
		deep.WindowMin = -2560
		deep.WindowMax = 2559
		if err := store.SetDeep(key, deep); err != nil {
			t.Fatal(err)
		}
		before, _ := store.Pool(key)
		for _, kind := range []uniswapv3.EventKind{uniswapv3.EventMint, uniswapv3.EventBurn} {
			event := uniswapv3.Event{Kind: kind, TickLower: bounds[0], TickUpper: bounds[1], Amount: big.NewInt(100)}
			if err := store.ApplyV3(key, event, 11); err != nil {
				t.Fatal(err)
			}
			if _, ok := store.View().Deep(key); !ok {
				t.Fatal("complete window invalidated by a reproducible liquidity event", bounds, kind)
			}
		}
		after, _ := store.Pool(key)
		if after.Liquidity.Cmp(before.Liquidity) != 0 {
			t.Fatal("mint/burn did not preserve active liquidity")
		}
		got, _ := store.View().Deep(key)
		for tick := range got.Ticks {
			if tick < got.WindowMin || tick > got.WindowMax {
				t.Fatal("partial tick outside the loaded window", tick)
			}
		}
	}
}

func TestLiquidityLogCannotMakeUnknownOrStaleTicksFresh(t *testing.T) {
	store := seededStore(t, testAddress(0x62))
	key := v3Key(testAddress(0x62))
	if err := store.ApplyV3(key, mintEvent(-60, 60, 100), 11); err != nil {
		t.Fatal(err)
	}
	if len(store.DeepStale()) != 1 {
		t.Fatal("unknown window became fresh")
	}
	deep, _ := store.Deep(key)
	deep.WindowMin = -2560
	deep.WindowMax = 2559
	// A deliberately invalidated snapshot retains its status after another log.
	store.deep[key] = deep
	if err := store.ApplyV3(key, burnEvent(-60, 60, 100), 11); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.View().Deep(key); ok {
		t.Fatal("liquidity log cleared an existing stale marker")
	}
}

func TestBurnWithMissingInWindowBoundaryInvalidatesWindow(t *testing.T) {
	store := seededStore(t, testAddress(0x63))
	key := v3Key(testAddress(0x63))
	deep, _ := store.Deep(key)
	deep.WindowMin = -2560
	deep.WindowMax = 2559
	delete(deep.Ticks, -120)
	if err := store.SetDeep(key, deep); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyV3(key, burnEvent(-120, 120, 100), 11); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.View().Deep(key); ok {
		t.Fatal("missing initialized boundary kept a window fresh")
	}
}
