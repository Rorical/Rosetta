package arb_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestRecordedMissingMarketsDirtySearchAndAmountOptimization(t *testing.T) {
	for _, name := range []string{"op-velodrome-cycle-158954686", "op-slipstream-cycle-158960654"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("..", "..", "testdata", "golden", name)
			read := func(name string) []byte {
				t.Helper()
				raw, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			var manifest struct{ Checksum map[string]string }
			if err := json.Unmarshal(read("manifest.json"), &manifest); err != nil {
				t.Fatal(err)
			}
			for file, want := range manifest.Checksum {
				digest := sha256.Sum256(read(file))
				if "sha256:"+hex.EncodeToString(digest[:]) != want {
					t.Fatalf("%s checksum mismatch", file)
				}
			}
			var expected struct {
				Cycle            graph.Cycle
				Input            string
				ActualGross      string            `json:"actual_gross"`
				ModelGross       string            `json:"model_gross_before_competitor"`
				ExecutionFees    map[string]uint32 `json:"execution_fees"`
				TransactionIndex int               `json:"transaction_index"`
				TriggerIndex     int               `json:"trigger_index"`
				After            string            `json:"gross_after_competitor"`
				Before           string            `json:"gross_before_trigger"`
			}
			if err := json.Unmarshal(read("expected.json"), &expected); err != nil {
				t.Fatal(err)
			}
			var record backtest.Record
			if err := json.Unmarshal(read("block.json"), &record); err != nil {
				t.Fatal(err)
			}
			integer := func(text string) *big.Int {
				t.Helper()
				v, ok := new(big.Int).SetString(text, 10)
				if !ok {
					t.Fatal(text)
				}
				return v
			}
			for _, compiled := range []bool{false, true} {
				t.Run(map[bool]string{true: "compiled", false: "fallback"}[compiled], func(t *testing.T) {
					store := state.New()
					if err := store.LoadSnapshot(read("snapshot.json")); err != nil {
						t.Fatal(err)
					}
					topology, err := pipeline.BuildTopology(t.Context(), store.Pools(), pipeline.TopologyOptions{Depth: 6, MaxCycles: 1000, K: 4, Notional: func(domain.Address) *big.Int { return integer(expected.Input) }})
					if err != nil {
						t.Fatal(err)
					}
					engine := topology.Engine
					if !compiled {
						q, e := simulate.NewQuoter(dex.NewAdapter)
						if e != nil {
							t.Fatal(e)
						}
						engine, err = arb.New(topology.Table, func(domain.Address) *big.Int { return integer(expected.Input) }, 4, q)
						if err != nil {
							t.Fatal(err)
						}
					}
					branch, err := ingest.NewTransactionBranch(store)
					if err != nil {
						t.Fatal(err)
					}
					if err = branch.Begin(record.Block); err != nil {
						t.Fatal(err)
					}
					quote := func(view arb.View, want string) {
						t.Helper()
						o, e := engine.QuoteCycle(t.Context(), view, expected.Cycle, integer(expected.Input))
						if e != nil {
							t.Fatal(e)
						}
						if o.Net.Cmp(integer(want)) != 0 {
							t.Fatalf("quote=%s want=%s", o.Net, want)
						}
					}
					search := func(view state.View, dirty []domain.PoolKey) {
						t.Helper()
						result, e := engine.EvaluateRanked(t.Context(), view, dirty, arb.RankedOptions{SwapFee: dex.SwapFee, Model: dex.RankingModel, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
							return arb.InputBounds{Min: big.NewInt(1_000_000_000), Max: big.NewInt(100_000_000_000_000_000)}, token == expected.Cycle.Start
						}})
						if e != nil {
							t.Fatal(e)
						}
						found := false
						for _, o := range result.Opportunities {
							if len(o.Cycle.Pools) == len(expected.Cycle.Pools) && o.Net.Sign() > 0 {
								found = true
								if o.Input.Sign() <= 0 {
									t.Fatal("invalid sized principal")
								}
							}
						}
						if !found {
							t.Fatalf("funded route not found: considered=%d selected=%d issues=%+v", result.Considered, result.Selected, result.Issues)
						}
					}
					quote(store.View(), expected.Before)
					if expected.TriggerIndex < 0 {
						search(store.View(), expected.Cycle.Pools)
					}
					for _, tx := range record.Transactions {
						if int(tx.Index) == expected.TransactionIndex {
							quote(branch.Working().View(), expected.ModelGross)
							// Independent fee() trace evidence separates dynamic fee
							// drift from pool math and receipt reconstruction errors.
							quote(recordedFeeView{View: branch.Working().View(), fees: expected.ExecutionFees}, expected.ActualGross)
							search(branch.Working().View(), expected.Cycle.Pools)
						}
						dirty, e := branch.Apply(t.Context(), tx)
						if e != nil {
							t.Fatal(e)
						}
						if int(tx.Index) == expected.TriggerIndex {
							if len(dirty) == 0 {
								t.Fatal("trigger did not dirty any pool")
							}
							search(branch.Working().View(), dirty)
						}
						if int(tx.Index) == expected.TransactionIndex {
							quote(branch.Working().View(), expected.After)
							if len(dirty) != len(expected.Cycle.Pools) {
								t.Fatalf("competitor updated %d/%d route pools", len(dirty), len(expected.Cycle.Pools))
							}
						}
					}
				})
			}
		})
	}
}

type recordedFeeView struct {
	arb.View
	fees map[string]uint32
}

func (v recordedFeeView) Pool(key domain.PoolKey) (pool.State, bool) {
	p, ok := v.View.Pool(key)
	if fee, exists := v.fees[key.Address.Hex()]; exists && ok {
		p.Fee = fee
	}
	return p, ok
}
