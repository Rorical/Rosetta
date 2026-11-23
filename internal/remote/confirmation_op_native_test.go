//go:build opnative && contracts

package remote

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func TestOPRPCConfirmationAgainstNativeExecution(t *testing.T) {
	for _, fork := range []string{"isthmus", "jovian"} {
		for _, mode := range []string{"token wallet", "native wallet", "native operator", "flash"} {
			t.Run(fork+"/"+mode, func(t *testing.T) {
				f := newOPFixture(t, fork)
				var first, second domain.PoolKey
				if mode != "token wallet" {
					first, second = f.nativePool(t, 2_000_000), f.nativePool(t, 600_000)
				}
				operator := addressAt(0x82)
				if mode == "native operator" {
					if err := f.engine.request(map[string]any{"method": "set", "accounts": map[string]any{operator.Hex(): map[string]any{"balance": "0x1000000000000000000000000000"}}}, nil); err != nil {
						t.Fatal(err)
					}
					f.call(t, "ArbitrageExecutor", f.executor, "setOperator", [20]byte(operator), true)
				}
				request := f.request(t)
				if mode == "native operator" {
					request.Parameters.Sender, request.Parameters.Nonce = operator, 0
				}
				want := int64(200)
				if mode != "token wallet" {
					request.Plan.Asset = domain.Address{}
					request.Plan.Steps[0].Pool, request.Plan.Steps[0].TokenIn, request.Plan.Steps[0].TokenOut = first, domain.Address{}, f.token1
					request.Plan.Steps[1].Pool, request.Plan.Steps[1].TokenIn, request.Plan.Steps[1].TokenOut = second, f.token1, domain.Address{}
				}
				if mode == "flash" {
					request.Plan.Funding = execution.FundingV3Flash
					request.Plan.Asset = f.token1
					request.Plan.Lender = domain.PoolKey{Kind: domain.PoolKindV3, Address: f.lender}
					request.Plan.LenderToken0 = false // the lender pays token1
					request.Plan.MaxLoanFee = big.NewInt(5)
					request.Plan.Steps[0].TokenIn, request.Plan.Steps[0].TokenOut = f.token1, domain.Address{}
					request.Plan.Steps[1].TokenIn, request.Plan.Steps[1].TokenOut = domain.Address{}, f.token1
					want = 195
				}
				node := &opRPC{fixture: f}
				client := node.server(t)
				snapshot, err := OpenSnapshot(t.Context(), client, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, domain.Hash{10}, domain.Deployments{})
				if err != nil {
					t.Fatal(err)
				}
				defer snapshot.Close()
				before, err := snapshot.Nonce(t.Context(), request.Parameters.Sender)
				if err != nil {
					t.Fatal(err)
				}
				proof, err := snapshot.Confirm(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				if proof.Profit.Int64() != want || proof.OP == nil || proof.Context.L1BlockNumber != 100 || proof.OP.OperatorFeeUpperBound.Cmp(proof.OP.OperatorFee) <= 0 {
					t.Fatal("incomplete OP confirmation", proof)
				}
				if err := snapshot.FeeModel().Validate(*proof); err != nil {
					t.Fatal(err)
				}
				call, err := execution.BuildExecutorCall(t.Context(), request.Plan, request.Parameters)
				if err != nil {
					t.Fatal(err)
				}
				args := executorArgs(call, 8453)
				envelope, err := execution.UnsignedDynamicFeeTx(call, 8453)
				if err != nil {
					t.Fatal(err)
				}
				args["envelope"] = "0x" + hex.EncodeToString(envelope)
				reference, err := f.execute(args, nil, false, true)
				output, decodeErr := hexData(reference.Output)
				if err != nil || !reference.Success || decodeErr != nil || len(output) != 64 || new(big.Int).SetBytes(output[:32]).Cmp(proof.Profit) != 0 || new(big.Int).SetBytes(output[32:]).Cmp(proof.LoanFee) != 0 {
					t.Fatal("native OP result differs", reference, err, decodeErr)
				}
				var nativeAccount struct{ Balance string }
				if err := f.engine.request(map[string]any{"method": "account", "address": call.Sender.Hex()}, &nativeAccount); err != nil {
					t.Fatal(err)
				}
				base, err := integer(nativeAccount.Balance)
				if err != nil {
					t.Fatal(err)
				}
				nativePost, err := integer(reference.Post[call.Sender.Hex()]["balance"].(string))
				if err != nil {
					t.Fatal(err)
				}
				charge := new(big.Int).Sub(base, nativePost)
				if request.Plan.Asset == (domain.Address{}) && proof.Owner == call.Sender {
					charge.Add(charge, proof.Profit)
				}
				if charge.Sign() <= 0 || charge.Cmp(proof.TotalFeeWei) > 0 {
					t.Fatal("OP fee reserve did not cover native charge", charge, proof.TotalFeeWei)
				}
				var vault struct{ Balance string }
				if err := f.engine.request(map[string]any{"method": "account", "address": opPredeploy(0x1b).Hex()}, &vault); err != nil {
					t.Fatal(err)
				}
				oldVault, err := integer(vault.Balance)
				if err != nil {
					t.Fatal(err)
				}
				newVault, err := integer(reference.Post[opPredeploy(0x1b).Hex()]["balance"].(string))
				if err != nil {
					t.Fatal(err)
				}
				if new(big.Int).Sub(newVault, oldVault).Cmp(proof.OP.OperatorFee) != 0 {
					t.Fatal("oracle operator fee differs from native vault charge")
				}
				dataCharge := new(big.Int).Sub(charge, proof.L2FeeWei)
				dataCharge.Sub(dataCharge, proof.OP.OperatorFee)
				if dataCharge.Sign() < 0 || dataCharge.Cmp(proof.OP.L1FeeUpperBound) > 0 {
					t.Fatal("native data charge escaped reserved bound", dataCharge)
				}

				gas, err := quantity(reference.GasUsed)
				if err != nil || gas != proof.GasUsed {
					t.Fatal("OP RPC gas differs from full native transaction", reference.GasUsed, proof.GasUsed, err)
				}
				after, err := snapshot.Nonce(t.Context(), request.Parameters.Sender)
				if err != nil || after != before || proof.NonceAfter != before+1 {
					t.Fatal("simulation changed canonical nonce", err)
				}
				if mode == "token wallet" && fork == "jovian" {
					node.fault.Store(5)
					modern, err := snapshot.Confirm(t.Context(), request)
					if err != nil || modern.OP.TraceMode != "fee-free" || modern.TotalFeeWei.Cmp(proof.TotalFeeWei) != 0 {
						t.Fatal("fee-free and refund-only modes diverged", err)
					}

					for fault, message := range map[int32]string{1: "fee-free tracing required", 2: "nonce", 3: "no longer canonical", 4: "fee bound", 6: "cannot fund"} {
						// Each fault changes the provider's answers at the same hash. Acquire
						// an independent reader so verified immutable reads from the successful
						// confirmation do not conceal the injected capability failure.
						node.fault.Store(0)
						fresh, err := OpenSnapshot(t.Context(), client, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, domain.Hash{10}, domain.Deployments{})
						if err != nil {
							t.Fatal(err)
						}
						node.fault.Store(fault)
						_, err = fresh.Confirm(t.Context(), request)
						fresh.Close()
						if err == nil || !strings.Contains(err.Error(), message) {
							t.Fatal("unsupported/inconsistent RPC accepted", fault, err)
						}
					}
				}
			})
		}
	}
}

func TestOPSharedPlanningSealsReadyAndEnforcesCompleteBudget(t *testing.T) {
	f := newOPFixture(t, "jovian")
	now := big.NewInt(1000)
	nativePrice := f.deploy(t, "TestRoundFeed", uint8(8), big.NewInt(200_000_000_000), now, now)
	assetPrice := f.deploy(t, "TestRoundFeed", uint8(8), big.NewInt(100_000_000), now, now)
	sequencer := f.deploy(t, "TestRoundFeed", uint8(0), new(big.Int), big.NewInt(100), now)
	f.call(t, "TestToken", f.token0, "setDecimals", uint8(6))
	request := f.request(t)
	client := (&opRPC{fixture: f}).server(t)
	snapshot, err := OpenSnapshot(t.Context(), client, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, domain.Hash{10}, domain.Deployments{})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	cfg := planning.Policy{Sender: f.owner, Executor: f.executor, CodeHash: request.Parameters.CodeHash, PolicyHash: domain.Hash{88}, GasLimit: 2_000_000, MaxFee: big.NewInt(2), Tip: new(big.Int), MaxTotalFee: big.NewInt(1_000_000_000_000), FeeMultiplier: 20_000, Slippage: 100, ValidSeconds: 30, MaxCandidates: 1, TimeoutMS: 1000, NativePrice: nativePrice, Sequencer: sequencer, Grace: 10, Assets: map[domain.Address]planning.AssetPolicy{f.token0: {MaxLoanFee: new(big.Int), MinNetProfit: big.NewInt(100), PriceFeed: assetPrice, MaxPriceAge: 60}}}
	store := poolstate.New()
	keys := []domain.PoolKey{request.Plan.Steps[0].Pool, request.Plan.Steps[1].Pool}
	for _, key := range keys {
		if err := store.Upsert(pool.State{Key: key, Token0: f.token0, Token1: f.token1, Fee: 3000, TickSpacing: 60, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1_000_000)}); err != nil {
			t.Fatal(err)
		}
	}
	version := domain.StateVersion{Source: domain.SourceStandard, Anchor: snapshot.Position().Block, Epoch: 1, BlockNumber: 10, PrefixHash: domain.Hash{10}}
	opportunity := arb.Opportunity{Cycle: graph.Cycle{Start: f.token0, Pools: keys, Len: 2}, Input: big.NewInt(1000), Output: big.NewInt(1200), Net: big.NewInt(200), Legs: []arb.Leg{{Pool: keys[0], AmountIn: big.NewInt(1000), AmountOut: big.NewInt(2000)}, {Pool: keys[1], AmountIn: big.NewInt(2000), AmountOut: big.NewInt(1200)}}}
	batch := planning.Batch{Version: version, Graph: 1, View: store.View(), Snapshot: snapshot, Opportunities: []arb.Opportunity{opportunity}}
	clock := func() time.Time { return time.Unix(1000, 0) }
	plan, rejected, err := planning.Build(t.Context(), batch, cfg, clock)
	if err != nil || plan == nil {
		t.Fatal("OP shared planning did not produce ready", err, rejected)
	}
	if plan.Economics.NativeFeeBudget.Cmp(big.NewInt(4_000_000)) <= 0 {
		t.Fatal("shared planner omitted external fees")
	}
	if err := execution.ValidateForDispatch(t.Context(), plan, version, 8453, 1, cfg.PolicyHash, 1_000_000); err != nil {
		t.Fatal(err)
	}
	cfg.MaxTotalFee = big.NewInt(4_000_000)
	plan, rejected, err = planning.Build(t.Context(), batch, cfg, clock)
	if err != nil || plan != nil || !strings.Contains(strings.Join(rejected, ";"), "complete fee budget") {
		t.Fatal("gas-only policy admitted OP external fees", plan, rejected, err)
	}
}
