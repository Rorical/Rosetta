package graph

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// A full OP catalog reproduces the production 500000-path cap. Both successful
// historical markets must survive the cap; tiny isolated-cycle fixtures cannot
// detect starvation by the dense core.
func TestRecordedOPCatalogKeepsThreeAndFiveHopMarketsAtCap(t *testing.T) {
	states := recordedOPPools(t)
	byKey := map[domain.PoolKey]pool.State{}
	for _, p := range states {
		byKey[p.Key] = p
	}
	g, err := Build(states)
	if err != nil {
		t.Fatal(err)
	}
	table, err := BuildBalancedTableContext(t.Context(), g, 6, 500000)
	if !errors.Is(err, ErrTableFull) || table.Len() != 500000 {
		t.Fatal("fixture did not reach live cap", err)
	}
	for _, name := range []string{"op-velodrome-cycle-158954686", "op-slipstream-cycle-158960654"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", name, "expected.json"))
		if err != nil {
			t.Fatal(err)
		}
		var row struct{ Cycle Cycle }
		if err = json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		current := row.Cycle.Start
		var tokens []domain.Address
		for _, key := range row.Cycle.Pools {
			p, ok := byKey[key]
			if !ok {
				t.Fatal("missing recorded pool", key)
			}
			tokens = append(tokens, current)
			switch current {
			case p.Token0:
				current = p.Token1
			case p.Token1:
				current = p.Token0
			default:
				t.Fatal("disconnected recorded path")
			}
		}
		origin, keys := canonicalCycle(row.Cycle.Start, row.Cycle.Pools, tokens)
		found := false
		for _, cycle := range table.cycles {
			if cycle.Start == origin && slices.Equal(cycle.Pools, keys) {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("recorded market omitted at live cap", name)
		}
	}
}

func recordedOPPools(t *testing.T) []pool.State {
	t.Helper()
	raw, err := os.ReadFile("testdata/op-mainnet-128-pools.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Pools []struct {
			Address, Token0, Token1 string
			Kind                    domain.PoolKind
			PoolID                  string `json:"pool_id"`
			Hooks                   string
		}
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	address := func(text string) domain.Address {
		t.Helper()
		v, e := domain.ParseAddress(text)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	var states []pool.State
	for _, r := range fixture.Pools {
		p := pool.State{Key: domain.PoolKey{Address: address(r.Address), Kind: r.Kind}, Token0: address(r.Token0), Token1: address(r.Token1)}
		if r.PoolID != "" {
			p.Key.PoolID, err = domain.ParseHash(r.PoolID)
			if err != nil {
				t.Fatal(err)
			}
		}
		if r.Hooks != "" {
			p.Key.Hooks = address(r.Hooks)
		}
		states = append(states, p)
	}
	return states
}
