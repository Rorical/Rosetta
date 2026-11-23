package solidlyv3_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/solidlyv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
)

// recorded holds eth_call outputs of three OP Mainnet Solidly V3 pools at
// pinned blocks 159011751-159011756 and the pool's own quoteSwap view results
// at the same block (see the "source" field of each file).
type recorded struct {
	Pool        string
	Block       uint64
	Token0      string
	Token1      string
	TickSpacing int32 `json:"tick_spacing"`
	Fee         uint32
	Tick        int32
	WordRadius  int16 `json:"word_radius"`
	Calls       map[string]string
	Quotes      []struct {
		ZeroForOne bool   `json:"zero_for_one"`
		AmountIn   string `json:"amount_in"`
		AmountOut  string `json:"amount_out"`
		TickAfter  int32  `json:"tick_after"`
	}
}

func (r recorded) CallContract(_ context.Context, to domain.Address, data []byte, block string, out *string) error {
	if block != fmt.Sprintf("0x%x", r.Block) || !equalAddress(to, r.Pool) {
		return fmt.Errorf("unrecorded snapshot %s at %s", to, block)
	}
	value, ok := r.Calls["0x"+hex.EncodeToString(data)]
	if !ok {
		return fmt.Errorf("unrecorded call 0x%x", data)
	}
	*out = value
	return nil
}

func equalAddress(a domain.Address, text string) bool {
	b, err := domain.ParseAddress(text)
	return err == nil && a == b
}

func mustAddress(t testing.TB, text string) domain.Address {
	t.Helper()
	a, err := domain.ParseAddress(text)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRecordedPoolsMatchOnChainQuoteSwap(t *testing.T) {
	files, err := filepath.Glob("testdata/op-*.json")
	if err != nil || len(files) == 0 {
		t.Fatal("no recorded fixtures", err)
	}
	crossed := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var r recorded
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		spec := solidlyv3.Spec{Address: mustAddress(t, r.Pool), Token0: mustAddress(t, r.Token0), Token1: mustAddress(t, r.Token1), TickSpacing: r.TickSpacing}
		state, deep, err := solidlyv3.Load(t.Context(), r, spec, fmt.Sprintf("0x%x", r.Block), r.WordRadius)
		if err != nil {
			t.Fatal(file, err)
		}
		if state.Key.Kind != domain.PoolKindSolidlyV3 || state.Fee != r.Fee || state.Tick != r.Tick {
			t.Fatal("slot0 fee or tick not decoded", file, state.Fee, state.Tick)
		}
		adapter, err := solidlyv3.New(state.TickSpacing)
		if err != nil {
			t.Fatal(err)
		}
		matched := 0
		for _, q := range r.Quotes {
			amount, _ := new(big.Int).SetString(q.AmountIn, 10)
			want, _ := new(big.Int).SetString(q.AmountOut, 10)
			tokenIn := state.Token1
			if q.ZeroForOne {
				tokenIn = state.Token0
			}
			got, err := adapter.QuoteExactIn(t.Context(), state, deep, tokenIn, amount)
			inWindow := q.TickAfter >= deep.WindowMin && q.TickAfter <= deep.WindowMax
			if err != nil {
				if !errors.Is(err, math.ErrOutOfWindow) || inWindow {
					t.Fatalf("%s %+v: %v", file, q, err)
				}
				continue
			}
			if got.Cmp(want) != 0 {
				t.Fatalf("%s %+v: local %s, on-chain %s", file, q, got, want)
			}
			matched++
			if q.TickAfter != r.Tick {
				crossed++
			}
		}
		if matched == 0 {
			t.Fatal("no in-window quote compared", file)
		}
	}
	if crossed == 0 {
		t.Fatal("no recorded quote moved the tick")
	}
}

func TestDecodeSetFee(t *testing.T) {
	data := make([]byte, 64)
	data[31], data[63] = 10, 50
	fee, matched, err := solidlyv3.DecodeSetFee([]domain.Hash{solidlyv3.TopicSetFee}, data)
	if err != nil || !matched || fee != 50 {
		t.Fatal("SetFee not decoded", fee, matched, err)
	}
	if _, matched, _ := solidlyv3.DecodeSetFee([]domain.Hash{{1}}, data); matched {
		t.Fatal("other event matched SetFee")
	}
	high := make([]byte, 64)
	high[60] = 0x02 // 33554432 pips, above the 100000 cap
	if _, _, err := solidlyv3.DecodeSetFee([]domain.Hash{solidlyv3.TopicSetFee}, high); err == nil {
		t.Fatal("fee above the pool cap accepted")
	}
	if _, _, err := solidlyv3.DecodeSetFee([]domain.Hash{solidlyv3.TopicSetFee}, data[:32]); err == nil {
		t.Fatal("short SetFee accepted")
	}
}
