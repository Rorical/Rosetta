package camelotv3

import (
	"context"
	"errors"
	"testing"
)

func TestTickTableWordRange(t *testing.T) {
	tests := []struct {
		name        string
		currentWord int32
		radius      int32
		spacing     int32
		wantFirst   int64
		wantLast    int64
		wantErr     bool
	}{
		{name: "zero", currentWord: 0, radius: 1, spacing: 10, wantFirst: -10, wantLast: 19},
		{name: "negative current word", currentWord: -1, radius: 1, spacing: 10, wantFirst: -20, wantLast: 9},
		{name: "negative last word floors", currentWord: -4, radius: 1, spacing: 10, wantFirst: -50, wantLast: -21},
		{name: "spacing one", currentWord: 0, radius: 1, spacing: 1, wantFirst: -1, wantLast: 1},
		{name: "upper bound", currentWord: 32767, radius: 1, spacing: 1, wantErr: true},
		{name: "lower bound", currentWord: -32768, radius: 1, spacing: 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first, last, err := tickTableWordRange(test.currentWord, test.radius, test.spacing)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidResponse) {
					t.Fatalf("error = %v, want ErrInvalidResponse", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("tickTableWordRange: %v", err)
			}
			if first != test.wantFirst || last != test.wantLast {
				t.Fatalf("range = [%d, %d], want [%d, %d]", first, last, test.wantFirst, test.wantLast)
			}
		})
	}
}

func TestRawTick(t *testing.T) {
	tests := []struct {
		name        string
		word        int64
		bit         int
		spacing     int32
		want        int32
		wantInvalid bool
	}{
		{name: "positive", word: 0, bit: 50, spacing: 10, want: 50},
		{name: "negative", word: -1, bit: 246, spacing: 10, want: -10},
		{name: "spacing one", word: 0, bit: 0, spacing: 1, want: 0},
		{name: "unaligned", word: 0, bit: 11, spacing: 10, wantInvalid: true},
		{name: "above int24", word: 32768, bit: 0, spacing: 1, wantInvalid: true},
		{name: "below int24", word: -32769, bit: 0, spacing: 1, wantInvalid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := rawTick(test.word, test.bit, test.spacing)
			if test.wantInvalid {
				if !errors.Is(err, ErrInvalidResponse) {
					t.Fatalf("error = %v, want ErrInvalidResponse", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("rawTick: %v", err)
			}
			if got != test.want {
				t.Fatalf("tick = %d, want %d", got, test.want)
			}
		})
	}
}

func TestLoadedBlockRef(t *testing.T) {
	tests := []struct {
		name  string
		block string
		want  uint64
	}{
		{name: "hex", block: "0x2a", want: 42},
		{name: "decimal", block: "42", want: 42},
		{name: "latest", block: "latest", want: 0},
		{name: "empty", block: "", want: 0},
		{name: "prefix only", block: "0x", want: 0},
		{name: "not a number", block: "0xzz", want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := loadedBlockRef(test.block).Number; got != test.want {
				t.Fatalf("number = %d, want %d", got, test.want)
			}
		})
	}
}

func TestDeepLoaderMethodForm(t *testing.T) {
	fake := newFakeCaller()
	setEmptyTickTable(fake, -10, 19)

	deep, err := (DeepLoader{Caller: fake}).LoadDeep(context.Background(), deepState(), "0x2a", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	if deep.LoadedBlock.Number != 42 {
		t.Fatalf("loaded block = %d, want 42", deep.LoadedBlock.Number)
	}
}

func TestDeepLoaderUsesBoundCaller(t *testing.T) {
	fake := newFakeCaller()
	setEmptyTickTable(fake, -10, 19)
	setTickTableBits(fake, 0, 10)
	fake.setOut(ticksCallData(10), algebraTicksResult(11, -11))

	deep, err := (DeepLoader{Caller: fake}).LoadDeep(context.Background(), deepState(), "0x2a", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	if len(deep.Ticks) != 1 {
		t.Fatalf("ticks = %d, want the one initialized tick", len(deep.Ticks))
	}

	state := deepState()
	calls := []recordedCall{
		callWithArgument(t, fake.calls, tickTableCall, 0),
		callWithArgument(t, fake.calls, ticksCall, 10),
	}
	for _, call := range calls {
		if call.to != state.Key.Address || call.block != "0x2a" {
			t.Fatalf("call = (%s, %s), want (%s, 0x2a)", call.to, call.block, state.Key.Address)
		}
	}
}
