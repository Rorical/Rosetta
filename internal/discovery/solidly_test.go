package discovery

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

var solidlyFactory = domain.Address{0x70, 0xfe, 0x4a, 0x44, 0xea, 0x50, 0x5c, 0xfa, 0x3a, 0x57, 0xb9, 0x5c, 0xf2, 0x86, 0x2d, 0x4f, 0xd5, 0xf0, 0xf6, 0x87}

// Real OP Mainnet PoolCreated logs from the Solidly V3 factory at blocks
// 115529904 and 115529936. Both pools were created at fee 100 and later moved
// by SetFee, so the creation fee is only a tier.
func solidlyCreations(t *testing.T) []rpc.Log {
	t.Helper()
	raw, err := os.ReadFile("testdata/solidly_v3_pool_created.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Address, BlockNumber, Data string
		Topics                     []string
	}
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) != 2 {
		t.Fatal("fixture", err)
	}
	var logs []rpc.Log
	for _, row := range rows {
		log := rpc.Log{}
		var err error
		if log.Address, err = domain.ParseAddress(row.Address); err != nil {
			t.Fatal(err)
		}
		if log.BlockNumber, err = strconv.ParseUint(strings.TrimPrefix(row.BlockNumber, "0x"), 16, 64); err != nil {
			t.Fatal(err)
		}
		if log.Data, err = hex.DecodeString(strings.TrimPrefix(row.Data, "0x")); err != nil {
			t.Fatal(err)
		}
		for _, text := range row.Topics {
			topic, err := domain.ParseHash(text)
			if err != nil {
				t.Fatal(err)
			}
			log.Topics = append(log.Topics, topic)
		}
		logs = append(logs, log)
	}
	return logs
}

func TestSolidlyV3CreationDecodesIndexedTickSpacing(t *testing.T) {
	deployments := &domain.Deployments{Factories: map[string]domain.Address{"solidly_v3": solidlyFactory, "uniswap_v3": {0x1f}}}
	for _, log := range solidlyCreations(t) {
		spec, ok, err := DecodeCreation(SourceSolidlyV3, log, deployments)
		if err != nil || !ok {
			t.Fatal("Solidly creation not decoded", err)
		}
		if spec.Kind != domain.PoolKindSolidlyV3 || spec.Fee != 100 || spec.TickSpacing != 1 || spec.Address != domain.Address(log.Data[44:64]) {
			t.Fatal("wrong Solidly fields", spec)
		}
		// Same topic0 as Uniswap V3, but a different emitter: not a V3 pool.
		if _, ok, _ := DecodeCreation(SourceUniswapV3, log, deployments); ok {
			t.Fatal("Solidly creation matched the Uniswap V3 source")
		}
		// The V3 decoder would read the indexed tick spacing as the fee.
		v3, _, _ := DecodeV3PoolCreated(log)
		if v3.Fee == spec.Fee {
			t.Fatal("fixture no longer distinguishes the layouts")
		}
	}
}

func TestProbeRecognizesSolidlyV3BySlot0FeeAndSpacingKey(t *testing.T) {
	pool, token0, token1 := domain.Address{0x2b, 0x6b}, domain.Address{0x0b}, domain.Address{0x7f}
	anchor := domain.BlockRef{Number: 100}
	caller := newFakeCaller()
	word := func(v int64) string { return wordHex(big.NewInt(v)) }
	addressWord := func(a domain.Address) string { return "0x" + strings.Repeat("00", 12) + hex.EncodeToString(a[:]) }
	caller.set(pool, probeSelector("factory()"), addressWord(solidlyFactory))
	caller.set(pool, probeSelector("token0()"), addressWord(token0))
	caller.set(pool, probeSelector("token1()"), addressWord(token1))
	caller.set(pool, probeSelector("tickSpacing()"), word(1))
	caller.set(pool, probeSelector("slot0()"), "0x"+strings.Repeat("0", 64)+strings.Repeat("0", 64)+strings.TrimPrefix(word(10), "0x")+strings.TrimPrefix(word(1), "0x"))
	getPool := make([]byte, 100)
	copy(getPool, probeSelector("getPool(address,address,int24)"))
	copy(getPool[16:36], token0[:])
	copy(getPool[48:68], token1[:])
	getPool[99] = 1
	caller.set(solidlyFactory, getPool, addressWord(pool))
	row, ok, err := probeMarket(t.Context(), caller, pool, []Emitter{{SourceSolidlyV3, solidlyFactory}}, anchor)
	if err != nil || !ok {
		t.Fatal("Solidly pool not recognized", err)
	}
	if row.Spec.Kind != domain.PoolKindSolidlyV3 || row.Spec.Fee != 10 || row.Spec.TickSpacing != 1 || row.Source != SourceSolidlyV3 {
		t.Fatal("wrong probed spec", row.Spec)
	}
}
