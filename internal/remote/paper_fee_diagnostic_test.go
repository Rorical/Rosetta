package remote

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type feeDriftLeg struct {
	Case, Leg                  int
	Pool, Token, Input, Output string
	Paper                      string `json:"paper_output"`
	Cascade                    string `json:"cascade_output"`
	Token0, Token1             string
	Sqrt                       string `json:"sqrt_price_x96"`
	Tick                       int32
	Liquidity                  string
	Fee                        uint32
	Cached                     uint32 `json:"cached_fee"`
	Kind                       domain.PoolKind
	Spacing                    int32 `json:"tick_spacing"`
	Bitmap                     map[string]string
	Ticks                      map[int32]pool.TickInfo
	Min                        int32 `json:"window_min"`
	Max                        int32 `json:"window_max"`
}

func feeDriftInteger(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("invalid fixture integer %q", s)
	}
	return n
}
func feeDriftAddress(t *testing.T, s string) domain.Address {
	t.Helper()
	a, err := domain.ParseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func TestHistoricalPrefixFeeDriftExplainsSixExecutorReverts(t *testing.T) {
	raw, err := os.ReadFile("testdata/op-slipstream-fee-drift.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Input string
			Gross string `json:"quoted_gross"`
		}
		Legs []feeDriftLeg
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 6 {
		t.Fatal("historical cases lost")
	}
	for index, c := range fixture.Cases {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			view := &dependencyView{shallow: map[domain.PoolKey]pool.State{}, deep: map[domain.PoolKey]pool.DeepState{}}
			fees := map[domain.PoolKey]uint32{}
			var keys []domain.PoolKey
			var start domain.Address
			var expected *big.Int
			changes := 0
			for _, v := range fixture.Legs {
				if v.Case != index {
					continue
				}
				key := domain.PoolKey{Address: feeDriftAddress(t, v.Pool), Kind: v.Kind}
				state := pool.State{Key: key, Token0: feeDriftAddress(t, v.Token0), Token1: feeDriftAddress(t, v.Token1), Fee: v.Fee, TickSpacing: v.Spacing, SqrtPriceX96: feeDriftInteger(t, v.Sqrt), Tick: v.Tick, Liquidity: feeDriftInteger(t, v.Liquidity)}
				deep := pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: v.Ticks, WindowMin: v.Min, WindowMax: v.Max}
				for k, b := range v.Bitmap {
					n, e := strconv.ParseInt(k, 10, 16)
					if e != nil {
						t.Fatal(e)
					}
					deep.Bitmap[int16(n)] = feeDriftInteger(t, b)
				}
				adapter, e := dex.NewAdapter(state)
				if e != nil {
					t.Fatal(e)
				}
				quoted, e := adapter.QuoteExactIn(t.Context(), state, deep, feeDriftAddress(t, v.Token), feeDriftInteger(t, v.Input))
				if e != nil || quoted.String() != v.Output {
					t.Fatalf("leg %d current-state quote=%v want=%s err=%v", v.Leg, quoted, v.Output, e)
				}
				fees[key] = v.Fee
				if v.Cached != v.Fee {
					changes++
				}
				state.Fee = v.Cached
				view.shallow[key], view.deep[key] = state, deep
				keys = append(keys, key)
				if v.Leg == 0 {
					start = feeDriftAddress(t, v.Token)
				}
				expected = feeDriftInteger(t, v.Cascade)
			}
			if changes != 1 {
				t.Fatalf("fee drift changed %d legs", changes)
			}
			original := arb.Opportunity{Cycle: graph.Cycle{Start: start, Pools: keys, Len: len(keys)}, Input: feeDriftInteger(t, c.Input)}
			stale, e := requotePaperFees(t.Context(), view, original, nil)
			if e != nil || stale.Net.String() != c.Gross || stale.Net.Sign() <= 0 {
				t.Fatalf("original positive quote not reproduced: %+v %v", stale, e)
			}
			updated, e := requotePaperFees(t.Context(), view, original, fees)
			if e != nil || updated.Output.Cmp(expected) != 0 || updated.Net.Sign() >= 0 {
				t.Fatalf("fee-adjusted result=%+v want output=%s err=%v", updated, expected, e)
			}
		})
	}
}

func TestPaperFeeDiagnosisUsesSenderPrefixAndCachesReads(t *testing.T) {
	var calls int
	sender := domain.Address{8}
	anchor := domain.Hash{9}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.Method != "eth_call" {
			t.Error(request.Method)
		}
		var call struct{ From, Data string }
		var block struct{ BlockHash string }
		var override map[string]any
		json.Unmarshal(request.Params[0], &call)
		json.Unmarshal(request.Params[1], &block)
		json.Unmarshal(request.Params[2], &override)
		if call.From != sender.Hex() || call.Data != "0xddca3f43" || block.BlockHash != anchor.Hex() || override[sender.Hex()] == nil {
			t.Error("fee read lost sender or prefix")
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": fmt.Sprintf("0x%064x", 365)})
	}))
	defer server.Close()
	store, pools := poolFixture(t)
	key := pools[0].Key
	key.Kind = domain.PoolKindSlipstream
	p := pools[0]
	p.Key = key
	p.Fee = 235
	view := &dependencyView{View: store.View(), shallow: map[domain.PoolKey]pool.State{key: p}}
	snapshot := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: domain.BlockRef{Hash: anchor}}, overlay: map[string]any{sender.Hex(): map[string]any{"balance": "0x1"}}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: p.Token0, Pools: []domain.PoolKey{key}}, Input: big.NewInt(10)}
	for range 2 {
		result := diagnosePaperFees(t.Context(), snapshot, view, o, sender)
		if result == nil || len(result.Changes) != 1 || result.Changes[0].Cached != 235 || result.Changes[0].Execution != 365 {
			t.Fatal(result)
		}
		// The fixture has no Slipstream ticks: keep missing state as an error.
		if result.Error == "" || result.Gross != nil {
			t.Fatal("missing deep state became a measured result")
		}
		o.Net = big.NewInt(500)
		job := paperExecutionJob{event: arb.SearchEvent{Job: arb.SearchJob{View: view}}, opportunity: o}
		executed := paperExecutionResult{Status: "success", Profit: big.NewInt(100), LoanFee: big.NewInt(5)}
		attachPaperFeeDiagnostic(t.Context(), snapshot, job, &executed, sender)
		if executed.Status != "success" || executed.Profit.Int64() != 100 || executed.FeeDiagnostic == nil || executed.FeeDiagnostic.Trigger != "gross_mismatch" || executed.FeeDiagnostic.ExecutedGross.Int64() != 105 || executed.FeeDiagnostic.Error == "" || executed.FeeDiagnostic.Residual != nil {
			t.Fatal("diagnostic changed the measured execution or hid missing ticks", executed)
		}
	}
	if calls != 1 {
		t.Fatalf("identical fee read repeated %d times", calls)
	}
}
