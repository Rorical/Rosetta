package discovery

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestDecodeV3PoolCreated(t *testing.T) {
	token0 := repeatedAddress(0x11)
	token1 := repeatedAddress(0x22)
	poolAddress := repeatedAddress(0x33)

	spec, ok, err := DecodeV3PoolCreated(v3Log(token0, token1, 500, 10, poolAddress))
	if err != nil {
		t.Fatalf("DecodeV3PoolCreated: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	want := Spec{
		Address:     poolAddress,
		Token0:      token0,
		Token1:      token1,
		Kind:        domain.PoolKindV3,
		Fee:         500,
		TickSpacing: 10,
	}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("spec = %+v, want %+v", spec, want)
	}
}

func TestDecodeV3PoolCreatedNegativeSpacing(t *testing.T) {
	spec, ok, err := DecodeV3PoolCreated(v3Log(repeatedAddress(1), repeatedAddress(2), 100, -60, repeatedAddress(3)))
	if err != nil || !ok {
		t.Fatalf("DecodeV3PoolCreated: ok = %v, err = %v", ok, err)
	}
	if spec.TickSpacing != -60 {
		t.Fatalf("tick spacing = %d, want -60", spec.TickSpacing)
	}
}

func TestDecodeSlipstreamPoolCreated(t *testing.T) {
	token0 := repeatedAddress(0x44)
	token1 := repeatedAddress(0x55)
	poolAddress := repeatedAddress(0x66)

	spec, ok, err := DecodeSlipstreamPoolCreated(slipstreamLog(token0, token1, 1, poolAddress))
	if err != nil {
		t.Fatalf("DecodeSlipstreamPoolCreated: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	want := Spec{
		Address:     poolAddress,
		Token0:      token0,
		Token1:      token1,
		Kind:        domain.PoolKindSlipstream,
		TickSpacing: 1,
	}
	if !reflect.DeepEqual(spec, want) {
		t.Fatalf("spec = %+v, want %+v", spec, want)
	}
}

func TestDecodeV4Initialize(t *testing.T) {
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)
	log := v4Log(t, token0, token1, 3000, 60, domain.Address{})

	spec, ok, err := DecodeV4Initialize(log)
	if err != nil {
		t.Fatalf("DecodeV4Initialize: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if spec.Address != v4PoolManager {
		t.Fatalf("address = %s, want PoolManager %s", spec.Address, v4PoolManager)
	}
	if spec.PoolID != log.Topics[1] {
		t.Fatalf("pool id = %s, want topic %s", spec.PoolID.Hex(), log.Topics[1].Hex())
	}
	if spec.Token0 != token0 || spec.Token1 != token1 {
		t.Fatalf("tokens = %s/%s, want %s/%s", spec.Token0, spec.Token1, token0, token1)
	}
	if spec.Kind != domain.PoolKindV4 || spec.Fee != 3000 || spec.TickSpacing != 60 {
		t.Fatalf("kind/fee/spacing = %d/%d/%d, want v4/3000/60", spec.Kind, spec.Fee, spec.TickSpacing)
	}
}

func TestDecodeV4InitializeHooked(t *testing.T) {
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)
	hooks := repeatedAddress(0x99)
	log := v4Log(t, token0, token1, 500, 10, hooks)

	spec, ok, err := DecodeV4Initialize(log)
	if err != nil || !ok {
		t.Fatalf("DecodeV4Initialize: ok = %v, err = %v", ok, err)
	}
	if spec.Hooks != hooks {
		t.Fatalf("hooks = %s, want %s", spec.Hooks, hooks)
	}
}

func TestDecodeWrongTopic(t *testing.T) {
	log := rpc.Log{
		Address: uniswapV3Factory,
		Topics:  []domain.Hash{{0x01}},
		Data:    make([]byte, 2*abiWordSize),
	}
	if _, ok, err := DecodeV3PoolCreated(log); ok || err != nil {
		t.Fatalf("v3: ok = %v, err = %v, want false, nil", ok, err)
	}
	if _, ok, err := DecodeSlipstreamPoolCreated(log); ok || err != nil {
		t.Fatalf("slipstream: ok = %v, err = %v, want false, nil", ok, err)
	}
	if _, ok, err := DecodeV4Initialize(log); ok || err != nil {
		t.Fatalf("v4: ok = %v, err = %v, want false, nil", ok, err)
	}
}

func TestDecodeMalformed(t *testing.T) {
	token0 := repeatedAddress(0x11)
	token1 := repeatedAddress(0x22)

	v3 := v3Log(token0, token1, 500, 10, repeatedAddress(0x33))
	slipstream := slipstreamLog(token0, token1, 1, repeatedAddress(0x44))
	v4 := v4Log(t, repeatedAddress(0x01), token1, 500, 10, domain.Address{})

	badAddressTopic := v3.Topics[1]
	badAddressTopic[0] = 0xff
	badFeeTopic := v3.Topics[3]
	badFeeTopic[28] = 0x01
	badSpacingWord := testInt24Word(10)
	badSpacingWord[0] = 0x01
	badHooksWord := testAddressWord(domain.Address{})
	badHooksWord[0] = 0x01
	badAddressWord := testAddressWord(repeatedAddress(0x33))
	badAddressWord[0] = 0x01

	tests := []struct {
		name  string
		log   rpc.Log
		check func(rpc.Log) (Spec, bool, error)
	}{
		{
			name:  "v3 short topics",
			log:   rpc.Log{Address: uniswapV3Factory, Topics: v3.Topics[:3], Data: v3.Data},
			check: DecodeV3PoolCreated,
		},
		{
			name:  "v3 short data",
			log:   rpc.Log{Address: uniswapV3Factory, Topics: v3.Topics, Data: v3.Data[:abiWordSize]},
			check: DecodeV3PoolCreated,
		},
		{
			name: "v3 bad address padding",
			log: rpc.Log{
				Address: uniswapV3Factory,
				Topics:  []domain.Hash{v3.Topics[0], badAddressTopic, v3.Topics[2], v3.Topics[3]},
				Data:    v3.Data,
			},
			check: DecodeV3PoolCreated,
		},
		{
			name: "v3 fee overflow",
			log: rpc.Log{
				Address: uniswapV3Factory,
				Topics:  []domain.Hash{v3.Topics[0], v3.Topics[1], v3.Topics[2], badFeeTopic},
				Data:    v3.Data,
			},
			check: DecodeV3PoolCreated,
		},
		{
			name:  "v3 bad tick spacing padding",
			log:   rpc.Log{Address: uniswapV3Factory, Topics: v3.Topics, Data: concatWords(badSpacingWord, testAddressWord(repeatedAddress(0x33)))},
			check: DecodeV3PoolCreated,
		},
		{
			name:  "v3 bad pool word",
			log:   rpc.Log{Address: uniswapV3Factory, Topics: v3.Topics, Data: concatWords(testInt24Word(10), badAddressWord)},
			check: DecodeV3PoolCreated,
		},
		{
			name:  "slipstream short topics",
			log:   rpc.Log{Address: slipstreamFactory, Topics: slipstream.Topics[:3], Data: slipstream.Data},
			check: DecodeSlipstreamPoolCreated,
		},
		{
			name:  "slipstream bad data length",
			log:   rpc.Log{Address: slipstreamFactory, Topics: slipstream.Topics, Data: make([]byte, 2*abiWordSize)},
			check: DecodeSlipstreamPoolCreated,
		},
		{
			name:  "v4 short data",
			log:   rpc.Log{Address: v4PoolManager, Topics: v4.Topics, Data: v4.Data[:4*abiWordSize]},
			check: DecodeV4Initialize,
		},
		{
			name: "v4 bad hooks padding",
			log: rpc.Log{
				Address: v4PoolManager,
				Topics:  v4.Topics,
				Data:    concatWords(testUtestInt24Word(500), testInt24Word(10), badHooksWord, testUintWord(1), testInt24Word(0)),
			},
			check: DecodeV4Initialize,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, ok, err := test.check(test.log)
			if ok {
				t.Fatal("ok = true, want false")
			}
			if !errors.Is(err, ErrInvalidLog) {
				t.Fatalf("error = %v, want ErrInvalidLog", err)
			}
		})
	}
}
