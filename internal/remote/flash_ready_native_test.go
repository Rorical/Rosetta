//go:build opnative && contracts

package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func TestFlashblocksSharedPlanningAndPrefixRetirement(t *testing.T) {
	f := newOPFixture(t, "jovian")
	now := big.NewInt(1002)
	nativePrice := f.deploy(t, "TestRoundFeed", uint8(8), big.NewInt(200_000_000_000), now, now)
	assetPrice := f.deploy(t, "TestRoundFeed", uint8(8), big.NewInt(100_000_000), now, now)
	sequencer := f.deploy(t, "TestRoundFeed", uint8(0), new(big.Int), big.NewInt(100), now)
	f.call(t, "TestToken", f.token0, "setDecimals", uint8(6))
	request := f.request(t)
	prefix, _ := nativeFlashPrefix(t, f)
	client := (&flashRPC{base: &opRPC{fixture: f, replay: true}}).server(t)
	parent, err := client.ExecutionBlock(t.Context(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	view, proof, err := openFlashSnapshot(t.Context(), client, parent, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, domain.Deployments{}, prefix)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	cfg := planning.Policy{Sender: f.owner, Executor: f.executor, CodeHash: request.Parameters.CodeHash, PolicyHash: domain.Hash{88}, GasLimit: 2_000_000, MaxFee: big.NewInt(2), Tip: new(big.Int), MaxTotalFee: big.NewInt(1_000_000_000_000), FeeMultiplier: 20_000, Slippage: 100, ValidSeconds: 30, MaxCandidates: 1, TimeoutMS: 1000, NativePrice: nativePrice, Sequencer: sequencer, Grace: 10, Assets: map[domain.Address]planning.AssetPolicy{f.token0: {MaxLoanFee: new(big.Int), MinNetProfit: big.NewInt(100), PriceFeed: assetPrice, MaxPriceAge: 60}}}
	store := poolstate.New()
	keys := []domain.PoolKey{request.Plan.Steps[0].Pool, request.Plan.Steps[1].Pool}
	for _, key := range keys {
		if err := store.Upsert(pool.State{Key: key, Token0: f.token0, Token1: f.token1, Fee: 3000, TickSpacing: 60, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1_000_000)}); err != nil {
			t.Fatal(err)
		}
	}
	opportunity := arb.Opportunity{Cycle: graph.Cycle{Start: f.token0, Pools: keys, Len: 2}, Input: big.NewInt(1000), Output: big.NewInt(1200), Net: big.NewInt(200), Legs: []arb.Leg{{Pool: keys[0], AmountIn: big.NewInt(1000), AmountOut: big.NewInt(2000)}, {Pool: keys[1], AmountIn: big.NewInt(2000), AmountOut: big.NewInt(1200)}}}
	batch := planning.Batch{Version: prefix.Version, Graph: 1, View: store.View(), Snapshot: view, Opportunities: []arb.Opportunity{opportunity}, FlashProof: proof}
	plan, rejections, err := planning.Build(t.Context(), batch, cfg, func() time.Time { return time.Unix(1002, 0) })
	if err != nil || plan == nil || plan.Flashblocks == nil || plan.FastReplay != nil {
		t.Fatal("Flashblocks shared planner failed", plan, rejections, err)
	}
	current := execution.FlashblocksDispatchContext{Version: prefix.Version, PayloadID: prefix.Payload, Index: prefix.Index, Source: prefix.Source}
	if err := execution.ValidateFlashblocksForDispatch(t.Context(), plan, current, 8453, 1, cfg.PolicyHash, 1_002_000); err != nil {
		t.Fatal(err)
	}
	// A result for a retired prefix must be discarded before serialization,
	// even when its full native execution was valid for the earlier position.
	workerCtx, cancel := context.WithCancel(t.Context())
	worker := pipeline.NewConfirmationWorker(workerCtx, time.Second)
	defer func() { cancel(); <-worker.Done() }()
	task := &emptyFlashTask{}
	identity := pipeline.ConfirmationIdentity{Version: prefix.Version, Graph: 1, ReceivedAt: time.Now()}
	if err := worker.Submit(pipeline.ConfirmationJob{Identity: identity, Task: task}); err != nil {
		t.Fatal(err)
	}
	event := <-worker.Results()
	var output bytes.Buffer
	runtime := &runtime{cfg: config.Config{Network: config.Network{ChainID: 8453}}, execution: cfg, tentative: prefix.Version, flash: flashCursor{active: true, payload: prefix.Payload, index: prefix.Index}, flashPrefix: prefix, flashConfirmation: worker, search: &pipeline.SearchScheduler{GraphVersion: 1}, plans: json.NewEncoder(&output)}
	runtime.invalidateTentative()
	event.Plan = plan
	if err := runtime.consumeFlashConfirmation(t.Context(), event); err != nil || output.Len() != 0 {
		t.Fatal("retired prefix escaped output gate", err)
	}
}

type emptyFlashTask struct{}

func (*emptyFlashTask) Run(context.Context) (*execution.ReadyPlan, []string, error) {
	return nil, nil, nil
}
func (*emptyFlashTask) Close() {}
