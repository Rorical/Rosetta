//go:build opnative && contracts

package remote

import (
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"math/big"
	"testing"
	"time"
)

func TestExecutionPaperVirtualConstructorAndExecution(t *testing.T) {
	f := newOPFixture(t, "jovian")
	node := &opRPC{fixture: f, paper: true}
	client := node.server(t)
	opened, err := OpenSnapshot(t.Context(), client, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, domain.Hash{10}, domain.Deployments{})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	s := opened.(*snapshot)
	artifact := f.artifacts["ArbitrageExecutor"]
	sender := addressAt(0xd1)
	executor, err := s.setupPaperExecutor(t.Context(), paperArtifact{abi: artifact.abi, code: artifact.code}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if executor == f.executor {
		t.Fatal("virtual executor reused the deployed address", executor)
	}
	request := f.request(t)
	request.Plan.Funding = execution.FundingV3FlashSwap
	request.Parameters.Sender = sender
	request.Parameters.Executor = executor
	request.Parameters.CodeHash, err = s.CodeHash(t.Context(), executor)
	if err != nil {
		t.Fatal(err)
	}
	request.Parameters.Nonce, err = s.Nonce(t.Context(), sender)
	if err != nil {
		t.Fatal(err)
	}
	var result paperExecutionResult
	if err := s.executePaper(t.Context(), request, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" || result.Profit.Int64() != 200 || result.TotalFee.Sign() <= 0 || result.GasUsed == 0 {
		t.Fatalf("incomplete success sample: %+v", result)
	}
	// Exact same virtual prestate, with an impossible minimum, must retain the
	// real revert payload and gas instead of discarding the failed experiment.
	request.Plan.MinProfit = big.NewInt(1000)
	var failure paperExecutionResult
	if err := s.executePaper(t.Context(), request, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Status != "reverted" || failure.GasUsed == 0 || failure.TotalFee.Sign() <= 0 || failure.RevertData == "" || failure.Evidence != nil {
		t.Fatalf("lost failed execution: %+v", failure)
	}
	node.fault.Store(7)
	var emptyFailure paperExecutionResult
	if err := s.executePaper(t.Context(), request, &emptyFailure); err != nil || emptyFailure.Status != "reverted" || emptyFailure.RevertData != "0x" {
		t.Fatal("empty revert payload lost gas evidence", emptyFailure, err)
	}
	node.fault.Store(0)
	var actual struct {
		Code, Balance string
		Nonce         uint64
	}
	if err := f.engine.request(map[string]any{"method": "account", "address": executor.Hex()}, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Code != "0x" || actual.Nonce != 0 {
		t.Fatal("paper deployed to canonical state", actual)
	}
	if err := f.engine.request(map[string]any{"method": "account", "address": sender.Hex()}, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Balance != "0x0" || actual.Nonce != 0 {
		t.Fatal("paper funded canonical sender", actual)
	}
}

func TestExecutionPaperFlashPrefixAndSetupCache(t *testing.T) {
	f := newOPFixture(t, "jovian")
	prefix, _ := nativeFlashPrefix(t, f)
	client := (&flashRPC{base: &opRPC{fixture: f, replay: true, paper: true}}).server(t)
	parent, err := client.ExecutionBlock(t.Context(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	request := f.request(t)
	store := poolstate.New()
	keys := []domain.PoolKey{request.Plan.Steps[0].Pool, request.Plan.Steps[1].Pool}
	for _, key := range keys {
		if err := store.Upsert(pool.State{Key: key, Token0: f.token0, Token1: f.token1, Fee: 3000, TickSpacing: 60, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1000000)}); err != nil {
			t.Fatal(err)
		}
	}
	// Net only feeds the fee preflight, which must clear the fixture's ~3.4e10 wei
	// fee; the executor's minimums come from the legs and its floor is 1 wei.
	opportunity := arb.Opportunity{Cycle: graph.Cycle{Start: f.token0, Pools: keys, Len: 2}, Input: big.NewInt(1000), Output: big.NewInt(1200), Net: big.NewInt(1e12), Legs: []arb.Leg{{Pool: keys[0], AmountIn: big.NewInt(1000), AmountOut: big.NewInt(2000)}, {Pool: keys[1], AmountIn: big.NewInt(2000), AmountOut: big.NewInt(1200)}}}
	artifact := f.artifacts["ArbitrageExecutor"]
	p := &executionPaper{client: client, chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, artifact: paperArtifact{abi: artifact.abi, code: artifact.code, hash: digest(artifact.code)}, policy: executionPaperPolicy{WrappedNative: f.token0.Hex(), GasLimit: 2000000, TipWei: "0", SlippageBPS: 50}}
	p.source.unsupported.Store(true) // Exercise native prefix replay without a node cache extension.
	job := paperExecutionJob{opportunity: opportunity, discovered: time.Now(), anchor: parent, prefix: &prefix, event: arb.SearchEvent{Job: arb.SearchJob{View: store.View(), Version: prefix.Version, ReceivedAt: time.Now()}}}
	for i := 0; i < 2; i++ {
		result := p.evaluate(t.Context(), job)
		if result.Status != "success" || result.Error != "" || result.Flash == nil || result.Evidence.Context.Timestamp != 1002 || result.Evidence.Context.Block.Number != 11 || result.Economics == nil || result.StateBasis != "discovery_snapshot" {
			t.Fatalf("prefix paper execution %d: %+v", i, result)
		}
		if len(p.setups) != 1 {
			t.Fatal("setup cache not retained")
		}
	}
	for _, entry := range p.setups {
		if len(entry.overlay) != 2 {
			t.Fatal("cached protocol state")
		}
		for address := range entry.overlay {
			if address == f.token0.Hex() || address == f.first.Hex() {
				t.Fatal("execution leaked into setup cache")
			}
		}
	}
}

func TestExecutionPaperReplaysShadowIntentAsConfiguredSender(t *testing.T) {
	f := newOPFixture(t, "jovian")
	prefix, _ := nativeFlashPrefix(t, f)
	client := (&flashRPC{base: &opRPC{fixture: f, replay: true, paper: true}}).server(t)
	parent, err := client.ExecutionBlock(t.Context(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	request := f.request(t)
	keys := []domain.PoolKey{request.Plan.Steps[0].Pool, request.Plan.Steps[1].Pool}
	opportunity := arb.Opportunity{Cycle: graph.Cycle{Start: f.token0, Pools: keys, Len: 2}, Input: big.NewInt(1000), Net: big.NewInt(200)}
	p := &executionPaper{client: client, chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, policy: executionPaperPolicy{WrappedNative: f.token0.Hex()}}
	p.source.unsupported.Store(true)
	intent := execution.ReactiveIntent{ID: domain.Hash{7}, Route: request.Plan, ExpectedNet: big.NewInt(200), MinimumNet: big.NewInt(100), Budget: big.NewInt(1)}
	intent.Call.CallParameters = request.Parameters
	intent.Call.Nonce = 0 // Shadow reserves no nonce; the replay reads it from state.
	job := paperExecutionJob{opportunity: opportunity, discovered: time.Now(), anchor: parent, prefix: &prefix, intent: &intent, event: arb.SearchEvent{Job: arb.SearchJob{View: poolstate.New().View(), Version: prefix.Version, ReceivedAt: time.Now()}}}
	result := p.evaluate(t.Context(), job)
	if result.Type != "reactive_verification" || result.Status != "success" || result.Profit.Int64() != 200 || result.StateBasis != "discovery_snapshot" || result.Intent.ID != intent.ID || result.Call.Sender != f.owner || result.Call.Executor != f.executor {
		t.Fatalf("shadow intent replay: %+v", result)
	}
	if len(p.setups) != 0 {
		t.Fatal("intent replay deployed a virtual executor")
	}
	intent.Route.MinProfit = big.NewInt(1000)
	if failed := p.evaluate(t.Context(), job); failed.Status != "reverted" || failed.RevertData == "" {
		t.Fatalf("unprofitable intent was not reported as a revert: %+v", failed)
	}
}
