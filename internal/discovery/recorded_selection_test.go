package discovery

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Real OP Mainnet fixed-block reserve vectors also exercise mixed token units
// in the market shortlist, rather than relying solely on equal-decimal examples.
func TestSelectionOnRecordedOptimismV2Reserves(t *testing.T) {
	raw, err := os.ReadFile("../catalog/testdata/optimism-v2-v4.json")
	if err != nil {
		t.Fatal(err)
	}
	// Pool fields in the capture use snake_case; keep the wire schema explicit.
	var wire struct {
		Calls []struct{ To, Input, Output string }
		Pools []struct {
			Address string          `json:"address"`
			Token0  string          `json:"token0"`
			Token1  string          `json:"token1"`
			Kind    domain.PoolKind `json:"kind"`
			Fee     uint32          `json:"fee"`
		}
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	reserves := map[string][]byte{}
	for _, call := range wire.Calls {
		if call.Input == "0x0902f1ac" {
			value, err := hex.DecodeString(strings.TrimPrefix(call.Output, "0x"))
			if err != nil {
				t.Fatal(err)
			}
			reserves[call.To] = value
		}
	}
	var pools []pool.State
	for _, p := range wire.Pools {
		if p.Kind != domain.PoolKindV2 {
			continue
		}
		address, _ := domain.ParseAddress(p.Address)
		a, _ := domain.ParseAddress(p.Token0)
		b, _ := domain.ParseAddress(p.Token1)
		raw := reserves[p.Address]
		if len(raw) != 96 {
			t.Fatal("missing recorded reserve ABI")
		}
		pools = append(pools, pool.State{Key: domain.PoolKey{Address: address, Kind: p.Kind}, Token0: a, Token1: b, Reserve0: new(big.Int).SetBytes(raw[:32]), Reserve1: new(big.Int).SetBytes(raw[32:64]), Fee: p.Fee})
	}
	if len(pools) != 3 {
		t.Fatal("real fixture coverage changed")
	}
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: pools[0].Token0.Hex(), Amount: "1"}}, MaxTokens: 3, MaxSlippageBPS: 9999, ActivityWindow: 100, ProbeLimit: 32}
	result, err := SelectMarkets(t.Context(), pools, nil, policy, 3, 3)
	if err != nil || len(result.Keys) != 3 {
		t.Fatal(result, err)
	}
}
