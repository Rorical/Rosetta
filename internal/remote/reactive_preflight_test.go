package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/poolrisk"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

// preflightServer answers each JSON-RPC method with a fixed result or error.
func preflightServer(t *testing.T, answers map[string]string) *rpc.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		answer, ok := answers[request.Method]
		if !ok {
			answer = `"error":{"code":-32601,"message":"method not found"}`
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,` + answer + `}`))
	}))
	t.Cleanup(server.Close)
	return rpc.NewClient(server.URL, rpc.Options{})
}

func preflightIntent() execution.ReactiveIntent {
	var intent execution.ReactiveIntent
	intent.Version = domain.StateVersion{Source: domain.SourceStandard, Anchor: domain.BlockRef{Number: 7, Hash: domain.Hash{7}}}
	intent.Call = execution.ExecutorCall{CallParameters: execution.CallParameters{Sender: domain.Address{1}, Executor: domain.Address{2}, GasLimit: 1_000_000, FeeCap: big.NewInt(10), TipCap: big.NewInt(0)}, Value: new(big.Int), Data: []byte{0xcd}}
	intent.Route.Steps = []execution.ExecutorStep{{Pool: domain.PoolKey{Address: domain.Address{3}}}}
	return intent
}

func TestPreflightDropsRevertsAndUnsimulatedIntents(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		revert, fail bool
	}{
		{name: "passes", answer: `"result":"0x"`},
		{name: "reverts with data", answer: `"error":{"code":3,"message":"execution reverted","data":"0x0174639d"}`, revert: true, fail: true},
		{name: "reverts without data", answer: `"error":{"code":-32000,"message":"execution reverted"}`, revert: true, fail: true},
		{name: "quote block unknown", answer: `"error":{"code":-32000,"message":"header not found"}`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := preflightServer(t, map[string]string{"eth_call": tc.answer})
			p := &reactivePreflight{read: client, trace: client, reverts: make(chan execution.ReactiveIntent, 1)}
			skipped, err := p.check(t.Context(), preflightIntent())
			if skipped || (err != nil) != tc.fail || errors.Is(err, errPreflightRevert) != tc.revert {
				t.Fatalf("check = %v", err)
			}
			if queued := len(p.reverts) == 1; queued != tc.revert {
				t.Fatalf("revert queued for diagnosis = %v", queued)
			}
		})
	}
	p := &reactivePreflight{}
	fast := preflightIntent()
	fast.Version.Source = domain.SourceFast
	if _, err := p.check(t.Context(), fast); err == nil || errors.Is(err, errPreflightRevert) {
		t.Fatal("a quote without a canonical block was simulated", err)
	}
}

func TestPreflightDiagnosisRecordsRefusedToken(t *testing.T) {
	token, pool := "0x22812d608854794add74efe25e9120e2bc529429", "0x0300000000000000000000000000000000000000"
	// The route pool 0x03… pays the executor 0x02….
	transfer := "0xa9059cbb" + strings.Repeat("0", 24) + "0200000000000000000000000000000000000000" + strings.Repeat("0", 62) + "64"
	frame := `{"type":"CALL","from":"0x01","to":"0x02","error":"execution reverted","calls":[{"type":"CALL","from":"` + pool + `","to":"` + pool + `","error":"execution reverted","calls":[{"type":"CALL","from":"` + pool + `","to":"` + token + `","input":"` + transfer + `","error":"execution reverted"}]}]}`
	client := preflightServer(t, map[string]string{
		"debug_traceCall": `"result":` + frame,
		"eth_call":        `"result":"0x` + strings.Repeat("0", 62) + `ff"`,
	})
	risks, err := tokenrisk.Open(filepath.Join(t.TempDir(), "risks.jsonl"), domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = risks.Close() })
	p := &reactivePreflight{trace: client, risks: risks}
	evidence, _, err := p.diagnose(t.Context(), preflightIntent())
	if err != nil || len(evidence) != 1 {
		t.Fatal("refused transfer not attributed", evidence, err)
	}
	address, _ := domain.ParseAddress(token)
	if !errors.Is(risks.Check(address), tokenrisk.ErrBlocked) {
		t.Fatal("refusing token not excluded")
	}
}

func TestPreflightTracesRouteOncePerMinute(t *testing.T) {
	p := &reactivePreflight{traced: map[string]time.Time{}}
	now := time.Now()
	intent := preflightIntent()
	if !p.claim(intent, now) || p.claim(intent, now.Add(time.Second)) || !p.claim(intent, now.Add(preflightRetraceAfter)) {
		t.Fatal("route trace throttle")
	}
}

func TestShadowIntentFailingPreflightIsNotAdmitted(t *testing.T) {
	r, event := reactiveFixture(t)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	<-r.reactive.jobs
	job := r.reactive.takeJob(r.reactive.primary())
	r.reactive.preflight = &reactivePreflight{read: preflightServer(t, map[string]string{"eth_call": `"error":{"code":3,"message":"execution reverted"}`})}
	if record := r.reactive.executeJob(t.Context(), job, r.reactive.primary()); record["status"] != "preflight_revert" || r.reactive.verificationPending() {
		t.Fatal("reverting shadow intent admitted", record["status"], record["error"])
	}
}

// A passed route skips the call until the pass expires; the same pools in the
// other direction, or with another lender, are a different route.
func TestPreflightSkipsARecentlyPassedRoute(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x"}`))
	}))
	t.Cleanup(server.Close)
	p := &reactivePreflight{read: rpc.NewClient(server.URL, rpc.Options{})}
	intent := preflightIntent()
	intent.Route.Steps[0].TokenIn, intent.Route.Steps[0].TokenOut = domain.Address{4}, domain.Address{5}
	for i, want := range []bool{false, true} {
		if skipped, err := p.check(t.Context(), intent); err != nil || skipped != want {
			t.Fatalf("check %d skipped = %v, %v", i, skipped, err)
		}
	}
	reverse := preflightIntent()
	reverse.Route.Steps[0].TokenIn, reverse.Route.Steps[0].TokenOut = domain.Address{5}, domain.Address{4}
	lender := preflightIntent()
	lender.Route.Steps[0] = intent.Route.Steps[0]
	lender.Route.Lender = domain.PoolKey{Address: domain.Address{9}}
	for _, other := range []execution.ReactiveIntent{reverse, lender} {
		if skipped, err := p.check(t.Context(), other); err != nil || skipped {
			t.Fatal("a different route reused the pass", err)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("eth_call count", calls.Load())
	}
	// Our transaction on the route reverted: the next intent is simulated.
	p.forget(intent)
	if skipped, err := p.check(t.Context(), intent); err != nil || skipped || calls.Load() != 4 {
		t.Fatal("a reverted route kept its pass", skipped, err)
	}
}

// A route made of legs that passed in other routes skips the call, under the
// same executor, funding source and leg directions. Its own revert takes the
// shortcut away even though the legs still pass elsewhere.
func TestPreflightComposesPassedLegs(t *testing.T) {
	leg := func(pool byte, in, out byte) execution.ExecutorStep {
		return execution.ExecutorStep{Pool: domain.PoolKey{Address: domain.Address{pool}}, TokenIn: domain.Address{in}, TokenOut: domain.Address{out}}
	}
	route := func(funding execution.Funding, steps ...execution.ExecutorStep) execution.ReactiveIntent {
		intent := preflightIntent()
		intent.Route.Funding, intent.Route.Asset, intent.Route.Steps = funding, domain.Address{10}, steps
		return intent
	}
	p := &reactivePreflight{}
	// A>B on pool 3 and B>A on pool 4 passed in two different routes.
	p.pass(routeKey(route(execution.FundingWallet, leg(3, 10, 11), leg(5, 11, 10))), time.Now())
	p.pass(routeKey(route(execution.FundingWallet, leg(6, 10, 11), leg(4, 11, 10))), time.Now())
	composed := route(execution.FundingWallet, leg(3, 10, 11), leg(4, 11, 10))
	if !p.proven(routeKey(composed)) {
		t.Fatal("a route of passed legs is not proven")
	}
	for name, other := range map[string]execution.ReactiveIntent{
		"reversed leg":     route(execution.FundingWallet, leg(4, 10, 11), leg(3, 11, 10)),
		"other funding":    route(execution.FundingV4Flash, leg(3, 10, 11), leg(4, 11, 10)),
		"flash-swap first": route(execution.FundingV3FlashSwap, leg(3, 10, 11), leg(4, 11, 10)),
		"unseen pool":      route(execution.FundingWallet, leg(3, 10, 11), leg(7, 11, 10)),
	} {
		if p.proven(routeKey(other)) {
			t.Fatal(name, "proven from legs that never ran that way")
		}
	}
	// Another route on pool 3's leg, proven only by composition.
	p.pass(routeKey(route(execution.FundingWallet, leg(8, 10, 12), leg(9, 12, 10))), time.Now())
	sibling := route(execution.FundingWallet, leg(3, 10, 11), leg(6, 11, 12), leg(9, 12, 10))
	p.pass(routeKey(route(execution.FundingWallet, leg(6, 11, 12))), time.Now())
	if !p.proven(routeKey(sibling)) {
		t.Fatal("sibling route not composed")
	}
	// The revert does not say which leg failed: every leg loses the shortcut.
	p.forget(composed)
	if p.proven(routeKey(composed)) || p.proven(routeKey(sibling)) {
		t.Fatal("a reverted route's legs still compose")
	}
	// A direct pass through pool 3 clears that leg, not the reverted route.
	p.pass(routeKey(route(execution.FundingWallet, leg(3, 10, 11), leg(5, 11, 10))), time.Now())
	p.pass(routeKey(route(execution.FundingWallet, leg(6, 10, 11), leg(4, 11, 10))), time.Now())
	if !p.proven(routeKey(sibling)) || p.proven(routeKey(composed)) {
		t.Fatal("direct passes should restore the legs but not the reverted route")
	}
	p.pass(routeKey(composed), time.Now())
	if !p.proven(routeKey(composed)) {
		t.Fatal("a direct pass does not clear the refutation")
	}
}

// Refutations survive a restart, so a revert stays fail-closed.
func TestPreflightRefutationsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reactive.jsonl.preflight.json")
	route := func(legs ...[3]byte) execution.ReactiveIntent {
		intent := preflightIntent()
		intent.Route.Steps = nil
		for _, l := range legs {
			intent.Route.Steps = append(intent.Route.Steps, execution.ExecutorStep{Pool: domain.PoolKey{Address: domain.Address{l[0]}}, TokenIn: domain.Address{l[1]}, TokenOut: domain.Address{l[2]}})
		}
		return intent
	}
	p := &reactivePreflight{store: path}
	p.pass(routeKey(route([3]byte{3, 4, 5}, [3]byte{7, 5, 4})), time.Now())
	p.pass(routeKey(route([3]byte{9, 4, 5}, [3]byte{8, 5, 4})), time.Now())
	composed := route([3]byte{3, 4, 5}, [3]byte{8, 5, 4})
	if !p.proven(routeKey(composed)) {
		t.Fatal("route not composed before the revert")
	}
	// Our route through pool 3 reverts; its pass, if any, and its legs go.
	p.forget(route([3]byte{3, 4, 5}, [3]byte{6, 5, 4}))
	p.save()
	restored := &reactivePreflight{store: path}
	restored.load(time.Now())
	if restored.proven(routeKey(composed)) {
		t.Fatal("a restored refuted leg composes")
	}
	if len(restored.refuted) != 1 || len(restored.refutedLegs) != 3 {
		t.Fatal("refutations not restored", len(restored.refuted), len(restored.refutedLegs))
	}
}

// Evicting a pass removes the legs it alone proved.
func TestPreflightEvictionDropsLegs(t *testing.T) {
	p := &reactivePreflight{}
	start := time.Now()
	first := preflightIntent()
	first.Route.Steps[0].TokenIn, first.Route.Steps[0].TokenOut = domain.Address{4}, domain.Address{5}
	p.pass(routeKey(first), start)
	for i := range preflightPassEntries {
		p.pass(fmt.Sprint(i), start.Add(time.Duration(i+1)*time.Second))
	}
	if len(p.legs) != 0 {
		t.Fatal("legs of an evicted pass survive", len(p.legs))
	}
}

// Passes survive a restart; a corrupt file only costs the passes.
func TestPreflightPassesPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reactive.jsonl.preflight.json")
	intent := preflightIntent()
	p := &reactivePreflight{store: path}
	p.pass(routeKey(intent), time.Now())
	p.save()
	restored := &reactivePreflight{store: path}
	restored.load(time.Now())
	if !restored.proven(routeKey(intent)) || restored.dirty {
		t.Fatal("pass not restored")
	}
	restored.forget(intent)
	restored.save()
	again := &reactivePreflight{store: path}
	again.load(time.Now())
	if again.proven(routeKey(intent)) {
		t.Fatal("forgotten pass came back")
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := &reactivePreflight{store: path}
	corrupt.load(time.Now())
	if len(corrupt.passed) != 0 {
		t.Fatal("corrupt store produced passes")
	}
}

// A full set drops its oldest pass, not all of them.
func TestPreflightPassesEvictOldest(t *testing.T) {
	p := &reactivePreflight{}
	start := time.Now()
	for i := range preflightPassEntries + 1 {
		p.pass(fmt.Sprint(i), start.Add(time.Duration(i)*time.Second))
	}
	if len(p.passed) != preflightPassEntries || p.proven("0") || !p.proven("1") || !p.proven(fmt.Sprint(preflightPassEntries)) {
		t.Fatal("eviction", len(p.passed))
	}
}

// A real preflight revert (InvalidSwap) on Robinhood: the HENS pool sent the
// executor 9,244.7 HENS and the executor's balance rose by 8,782.5. The taxed
// token is recorded; WETH and USDG on the same route are protected.
func TestPreflightDiagnosisRecordsTaxedToken(t *testing.T) {
	raw, err := os.ReadFile("testdata/robinhood-hens-preflight-trace.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Block struct {
			Number uint64
			Hash   string
		}
		Executor   string
		RoutePools []string `json:"route_pools"`
		Token      string
		Trace      json.RawMessage
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	client := preflightServer(t, map[string]string{"debug_traceCall": `"result":` + string(fixture.Trace)})
	risks, err := tokenrisk.Open(filepath.Join(t.TempDir(), "risks.jsonl"), domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 4663})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = risks.Close() })
	weth, _ := domain.ParseAddress("0x0bd7d308f8e1639fab988df18a8011f41eacad73")
	usdg, _ := domain.ParseAddress("0x5fc5360d0400a0fd4f2af552add042d716f1d168")
	p := &reactivePreflight{trace: client, risks: risks, protected: map[domain.Address]bool{weth: true, usdg: true}}
	intent := preflightIntent()
	intent.Call.Executor, _ = domain.ParseAddress(fixture.Executor)
	hash, _ := domain.ParseHash(fixture.Block.Hash)
	intent.Version.Anchor = domain.BlockRef{Number: fixture.Block.Number, Hash: hash}
	intent.Route.Steps = nil
	token, _ := domain.ParseAddress(fixture.Token)
	for i, text := range fixture.RoutePools {
		address, _ := domain.ParseAddress(text)
		out := []domain.Address{token, usdg, weth}[i]
		intent.Route.Steps = append(intent.Route.Steps, execution.ExecutorStep{Pool: domain.PoolKey{Address: address}, TokenOut: out})
	}
	evidence, _, err := p.diagnose(t.Context(), intent)
	if err != nil || len(evidence) != 1 || evidence[0].Token != token || evidence[0].Received.Cmp(evidence[0].Requested) >= 0 {
		t.Fatal("taxed transfer not attributed", evidence, err)
	}
	if !errors.Is(risks.Check(token), tokenrisk.ErrBlocked) || risks.Check(weth) != nil || risks.Check(usdg) != nil {
		t.Fatal("wrong tokens excluded")
	}
}

// fastProbeIntent is an encodable two-pool route quoted on a Fast prefix.
func fastProbeIntent(t *testing.T) execution.ReactiveIntent {
	t.Helper()
	a, b := domain.Address{0x0a}, domain.Address{0x0b}
	plan := execution.ExecutorPlan{Funding: execution.FundingWallet, Asset: a, Amount: big.NewInt(100), MaxLoanFee: new(big.Int), MinProfit: big.NewInt(5), Deadline: uint64(time.Now().Unix() + 2),
		Steps: []execution.ExecutorStep{
			{Pool: domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{0x03}}, TokenIn: a, TokenOut: b, Fee: 3000, MinAmountOut: big.NewInt(90)},
			{Pool: domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{0x04}}, TokenIn: b, TokenOut: a, Fee: 3000, MinAmountOut: big.NewInt(105)},
		}}
	params := execution.CallParameters{Sender: domain.Address{1}, Executor: domain.Address{2}, CodeHash: domain.Hash{9}, GasLimit: 300_000, FeeCap: big.NewInt(10), TipCap: new(big.Int)}
	call, err := execution.BuildExecutorCall(t.Context(), plan, params)
	if err != nil {
		t.Fatal(err)
	}
	var intent execution.ReactiveIntent
	intent.Route, intent.Call = plan, call
	intent.Version = domain.StateVersion{Source: domain.SourceFast, Anchor: domain.BlockRef{Number: 7, Hash: domain.Hash{7}}, BlockNumber: 8, TxIndex: 3}
	return intent
}

// A Fast quote has no written block to call at: an unproven route is dropped
// and proven in the background at the written Fast block with its price
// guards removed. Only a call that succeeds proves it, after which the
// route's Fast intents send at once. A profit or output revert is
// inconclusive (its data can come from any contract on the route), and any
// other revert goes to the trace diagnosis at that block.
func TestFastIntentProvesItsRouteInTheBackground(t *testing.T) {
	written := `"result":{"number":"0x8","hash":"` + domain.Hash{8}.Hex() + `","parentHash":"` + domain.Hash{7}.Hex() + `","timestamp":"0x1","baseFeePerGas":"0x1","gasLimit":"0x1000000","miner":"` + domain.Address{}.Hex() + `","transactions":[]}`
	for _, tc := range []struct {
		name, answer      string
		proven, diagnosed bool
	}{
		{name: "succeeds", answer: `"result":"0x"`, proven: true},
		{name: "unprofitable at the block", answer: `"error":{"code":3,"message":"execution reverted","data":"0x4e47f8ea"}`},
		{name: "profit revert with extra data", answer: `"error":{"code":3,"message":"execution reverted","data":"0x4e47f8ea0000"}`},
		{name: "swap reverts", answer: `"error":{"code":3,"message":"execution reverted","data":"0x11157667"}`, diagnosed: true},
		{name: "node unavailable", answer: `"error":{"code":-32000,"message":"header not found"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := preflightServer(t, map[string]string{"eth_call": tc.answer, "eth_getBlockByNumber": written})
			p := &reactivePreflight{read: client, trace: client, reverts: make(chan execution.ReactiveIntent, 1), probes: make(chan execution.ReactiveIntent, 1)}
			intent := fastProbeIntent(t)
			if skipped, err := p.check(t.Context(), intent); skipped || !errors.Is(err, errPreflightUnproven) || len(p.probes) != 1 {
				t.Fatal("unproven Fast intent", skipped, err)
			}
			p.probe(t.Context(), <-p.probes)
			if p.proven(routeKey(intent)) != tc.proven || (len(p.reverts) == 1) != tc.diagnosed {
				t.Fatal("proven", p.proven(routeKey(intent)), "diagnosed", len(p.reverts))
			}
			if tc.proven {
				if skipped, err := p.check(t.Context(), intent); !skipped || err != nil {
					t.Fatal("proven route still held", skipped, err)
				}
			}
			if tc.diagnosed {
				d := <-p.reverts
				if bytes.Equal(d.Call.Data, intent.Call.Data) || d.Version.Anchor.Hash != (domain.Hash{8}) {
					t.Fatal("diagnosis must trace the probed call at the written block")
				}
			}
		})
	}
}

func TestPreflightRevertsQuarantineUnprovenPools(t *testing.T) {
	chain := domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: domain.ArbitrumOneChainID}
	healthy, suspect, lender := domain.PoolKey{Address: domain.Address{0x10}}, domain.PoolKey{Address: domain.Address{0x11}}, domain.PoolKey{Address: domain.Address{0x12}}
	trapToken := domain.Address{0x30}
	route := func(tail byte, out domain.Address, keys ...domain.PoolKey) execution.ReactiveIntent {
		intent := preflightIntent()
		intent.Route.Lender = lender
		intent.Route.Steps = nil
		for _, key := range keys {
			intent.Route.Steps = append(intent.Route.Steps, execution.ExecutorStep{Pool: key})
		}
		intent.Route.Steps = append(intent.Route.Steps, execution.ExecutorStep{Pool: domain.PoolKey{Address: domain.Address{0x20, tail}}, TokenOut: out})
		return intent
	}
	reverting := preflightServer(t, map[string]string{"eth_call": `"error":{"code":3,"message":"execution reverted","data":"0x4e47f8ea"}`})
	plainTrace := preflightServer(t, map[string]string{"debug_traceCall": `"result":{"type":"CALL","error":"execution reverted"}`})
	brokenTrace := preflightServer(t, map[string]string{})
	for _, tc := range []struct {
		name        string
		trace       *rpc.Client
		out         domain.Address
		quarantined bool
	}{
		{name: "revert the trace does not pin on a token", trace: plainTrace, quarantined: true},
		{name: "revert the trace cannot explain", trace: brokenTrace},
		{name: "route holding an excluded token", trace: plainTrace, out: trapToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pools, err := poolrisk.Open("", chain)
			if err != nil {
				t.Fatal(err)
			}
			risks, err := tokenrisk.Open("", chain)
			if err != nil {
				t.Fatal(err)
			}
			if err := risks.Record(tokenrisk.Evidence{Token: trapToken, Requested: big.NewInt(10), Received: big.NewInt(0), Block: domain.BlockRef{Number: 7, Hash: domain.Hash{7}}, Source: "executor_trace"}); err != nil {
				t.Fatal(err)
			}
			p := &reactivePreflight{pools: pools, risks: risks, trace: tc.trace, traceWait: time.Millisecond, read: preflightServer(t, map[string]string{"eth_call": `"result":"0x"`})}
			if _, err := p.check(t.Context(), route(0, domain.Address{}, healthy)); err != nil {
				t.Fatal(err)
			}
			p.read = reverting
			for i := range 3 {
				intent := route(byte(i+1), tc.out, healthy, suspect)
				if _, err := p.check(t.Context(), intent); !errors.Is(err, errPreflightRevert) {
					t.Fatalf("check %d: %v", i, err)
				}
				p.diagnoseRevert(t.Context(), intent)
			}
			if got := pools.Check(suspect) != nil; got != tc.quarantined {
				t.Fatalf("suspect quarantined = %v, want %v", got, tc.quarantined)
			}
			if pools.Check(healthy) != nil || pools.Check(lender) != nil {
				t.Fatal("a pool that passed, or the flash lender, was quarantined")
			}
		})
	}
}

// A V3 flash whose lender cannot send the loan reverts in the lender's asset
// transfer (a WETH/ARB pool holding 2.8e15 wei asked for a 7.5e16 loan);
// that is not the route's fault. A failure elsewhere is.
func TestLenderTransferFailureIsNotTheRoutesFault(t *testing.T) {
	weth, lender := domain.Address{0xe1}, domain.Address{0x01}
	plan := execution.ExecutorPlan{Funding: execution.FundingV3Flash, Lender: domain.PoolKey{Address: lender, Kind: domain.PoolKindV3}, Asset: weth, Amount: big.NewInt(75490979607566508)}
	trace := func(failing string) tokenrisk.CallFrame {
		transfer := tokenrisk.CallFrame{Type: "CALL", To: strings.ToLower(weth.Hex()), Input: "0xa9059cbb" + strings.Repeat("0", 128), Error: "execution reverted"}
		lenderCall := tokenrisk.CallFrame{Type: "CALL", To: strings.ToUpper(lender.Hex()[:2]) + lender.Hex()[2:], Input: "0x490e6cbc", Error: "execution reverted", Calls: []tokenrisk.CallFrame{transfer}}
		if failing == "swap" {
			lenderCall = tokenrisk.CallFrame{Type: "CALL", To: lender.Hex(), Input: "0x490e6cbc", Error: "execution reverted", Calls: []tokenrisk.CallFrame{{Type: "CALL", To: domain.Address{0x77}.Hex(), Input: "0x128acb08", Error: "execution reverted"}}}
		}
		return tokenrisk.CallFrame{Type: "CALL", To: domain.Address{0xe0}.Hex(), Error: "execution reverted", Calls: []tokenrisk.CallFrame{lenderCall}}
	}
	if !lenderTransferFailed(trace("lender"), plan) {
		t.Fatal("a failed lender transfer was blamed on the route")
	}
	if lenderTransferFailed(trace("swap"), plan) {
		t.Fatal("a swap failure inside the flash callback was excused")
	}
	callback := plan
	callback.Funding, callback.Lender = execution.FundingV3FlashSwap, domain.PoolKey{}
	if lenderTransferFailed(trace("lender"), callback) {
		t.Fatal("a route without a standalone lender was excused")
	}
}

// Shadow mode has no receipts, so expected gas falls back to 60% of the limit
// on every route, while routes typically use about half. A route
// that passes, or is skipped as proven, is traced off the send path and its
// gas joins the route history: a Standard intent at its quote block, a Fast
// one at its written block. With the Fast Feed on every intent is Fast and
// most routes are proven before the process starts, so measuring only new
// passes would learn nothing. A traced revert is not a measurement.
func TestPassedRoutesMeasureTheirGas(t *testing.T) {
	type sample struct {
		pools   int
		funding execution.Funding
		used    uint64
	}
	for _, tc := range []struct {
		name, trace string
		want        int
	}{
		{name: "traced", trace: `"result":{"type":"CALL","gasUsed":"0x3fefd"}`, want: 3},
		{name: "trace reverts", trace: `"result":{"type":"CALL","gasUsed":"0x1000","error":"execution reverted"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			written := `"result":{"number":"0x8","hash":"` + domain.Hash{8}.Hex() + `","parentHash":"` + domain.Hash{7}.Hex() + `","timestamp":"0x1","baseFeePerGas":"0x1","gasLimit":"0x1000000","miner":"` + domain.Address{}.Hex() + `","transactions":[]}`
			client := preflightServer(t, map[string]string{"eth_call": `"result":"0x"`, "debug_traceCall": tc.trace, "eth_getBlockByNumber": written})
			var got []sample
			p := &reactivePreflight{read: client, trace: client, reverts: make(chan execution.ReactiveIntent, 1), probes: make(chan execution.ReactiveIntent, 1), measures: make(chan gasMeasure, 4),
				gas: func(route []domain.PoolKey, funding execution.Funding, used uint64) {
					got = append(got, sample{len(route), funding, used})
				}}
			standard := preflightIntent()
			standard.Route.Funding = execution.FundingV3FlashSwap
			for range 2 { // the second check skips a proven route within its throttle
				if _, err := p.check(t.Context(), standard); err != nil {
					t.Fatal(err)
				}
			}
			fast := fastProbeIntent(t)
			if _, err := p.check(t.Context(), fast); !errors.Is(err, errPreflightUnproven) {
				t.Fatal(err)
			}
			p.probe(t.Context(), <-p.probes)
			proven := fastProbeIntent(t)
			proven.Route.Steps[0].Pool.Address = domain.Address{0x05}
			p.pass(routeKey(proven), time.Now())
			if skipped, err := p.check(t.Context(), proven); !skipped || err != nil {
				t.Fatal("proven Fast route", skipped, err)
			}
			if len(p.measures) != 3 {
				t.Fatal("measurements queued", len(p.measures))
			}
			for len(p.measures) > 0 {
				p.measureGas(t.Context(), <-p.measures)
			}
			want := []sample{{1, execution.FundingV3FlashSwap, 261885}, {2, execution.FundingWallet, 261885}, {2, execution.FundingWallet, 261885}}
			if len(got) != tc.want || tc.want > 0 && (got[0] != want[0] || got[1] != want[1] || got[2] != want[2]) {
				t.Fatal("route gas", got)
			}
		})
	}
}

// Measurements spend paid provider requests, so each route is measured at
// most once per ten minutes and the process at most six times a minute.
func TestRouteGasMeasurementsAreThrottled(t *testing.T) {
	p := &reactivePreflight{trace: &rpc.Client{}, measures: make(chan gasMeasure, 16), gas: func([]domain.PoolKey, execution.Funding, uint64) {}}
	for i := range 10 {
		intent := preflightIntent()
		intent.Route.Steps[0].Pool.Address = domain.Address{byte(10 + i)}
		p.queueMeasure(intent, intent.Call, domain.Hash{7})
		p.queueMeasure(intent, intent.Call, domain.Hash{7})
	}
	if len(p.measures) != 10 {
		t.Fatal("measurements in a minute", len(p.measures))
	}
}

func TestUnprovenRouteSendsWhenNetCoversTheFeeBudget(t *testing.T) {
	intent := func(net int64) execution.ReactiveIntent {
		i := preflightIntent()
		i.Version.Source = domain.SourceFast
		// Two asset units per native unit: a net of 2,000 is 1,000 native.
		i.Budget, i.ExpectedNet = big.NewInt(1000), big.NewInt(net)
		i.Conversion = execution.FeeConversion{Numerator: big.NewInt(2), Denominator: big.NewInt(1)}
		return i
	}
	p := &reactivePreflight{unprovenCover: 10000}
	if !p.covers(intent(2000)) || p.covers(intent(1999)) {
		t.Fatal("cover must compare the net in native with the whole fee budget")
	}
	if (&reactivePreflight{}).covers(intent(1 << 40)) {
		t.Fatal("cover is off unless configured")
	}
	// A route that reverted for us, or shares a leg with one, needs a real pass.
	sibling := intent(2000)
	sibling.Route.Steps = append(sibling.Route.Steps, execution.ExecutorStep{Pool: domain.PoolKey{Address: domain.Address{9}}})
	p.forget(sibling)
	if p.covers(sibling) || p.covers(intent(2000)) {
		t.Fatal("a refuted route or leg was covered")
	}
}

func TestSettlementsProveAndLostRacesRestoreRoutes(t *testing.T) {
	route := func(pools ...byte) execution.ReactiveIntent {
		i := preflightIntent()
		i.Route.Steps = nil
		for _, pool := range pools {
			i.Route.Steps = append(i.Route.Steps, execution.ExecutorStep{Pool: domain.PoolKey{Address: domain.Address{pool}}})
		}
		return i
	}
	p := &reactivePreflight{}
	now := time.Now()
	// A transaction that succeeded on chain ran every leg.
	p.settled(route(3, 4), domain.Hash{1}, true, now)
	p.settled(route(5, 6), domain.Hash{2}, true, now)
	composed := route(3, 6)
	if !p.proven(routeKey(route(3, 4))) || !p.proven(routeKey(composed)) {
		t.Fatal("an on-chain success did not prove its route and legs")
	}
	// A revert refutes until its diagnosis; a lost race then undoes exactly that.
	p.settled(route(3, 4), domain.Hash{3}, false, now)
	if p.proven(routeKey(route(3, 4))) || p.proven(routeKey(composed)) {
		t.Fatal("a reverted route stayed proven before its diagnosis")
	}
	p.diagnosed(domain.Hash{3}, "pool_moved:1")
	if !p.proven(routeKey(route(3, 4))) || !p.proven(routeKey(composed)) || len(p.refuted) != 0 || len(p.refutedLegs) != 0 {
		t.Fatal("a lost race left the route refuted")
	}
	// Any other diagnosis keeps the refutation, and a second answer changes nothing.
	p.settled(route(3, 4), domain.Hash{4}, false, now)
	p.diagnosed(domain.Hash{4}, "no_transfer_shortfall_evidence")
	p.diagnosed(domain.Hash{4}, "pool_moved:0")
	if p.proven(routeKey(route(3, 4))) || p.proven(routeKey(composed)) {
		t.Fatal("an unexplained revert was forgiven")
	}
	// A second revert before the first diagnosis: undoing the first must not
	// lift what the second, unexplained one refuted.
	p.settled(route(5, 6), domain.Hash{6}, false, now)
	p.settled(route(5, 9), domain.Hash{7}, false, now)
	p.settled(route(5, 6), domain.Hash{8}, false, now)
	p.diagnosed(domain.Hash{6}, "pool_moved:0")
	if p.proven(routeKey(route(5, 6))) || len(p.refutedLegs) == 0 {
		t.Fatal("a stale undo lifted later refutations")
	}
	// A never-proven route that lost a race is unproven again, not proven.
	fresh := route(7, 8)
	p.settled(fresh, domain.Hash{5}, false, now)
	p.diagnosed(domain.Hash{5}, "pool_moved:0")
	if _, refuted := p.refuted[routeKey(fresh)]; refuted || p.proven(routeKey(fresh)) {
		t.Fatal("a lost race on an unproven route must leave it merely unproven")
	}
}

// Passes keyed by the executor address would all stop counting when the
// executor is redeployed, and most intents would be dropped as unproven. A pass
// says a route's pools and tokens let our executor's legs through, and a new
// executor runs the same legs.
func TestPassesCarryOverToANewExecutor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reactive.jsonl.preflight.json")
	old := preflightIntent()
	p := &reactivePreflight{store: path}
	p.pass(routeKey(old), time.Now())
	refuted := preflightIntent()
	refuted.Route.Steps[0].Pool.Address = domain.Address{0x77}
	p.forget(refuted)
	p.save()

	moved := preflightIntent()
	moved.Call.Executor = domain.Address{0x3d, 0x03}
	restored := &reactivePreflight{store: path, executor: moved.Call.Executor}
	restored.load(time.Now())
	if !restored.proven(routeKey(moved)) {
		t.Fatal("a pass under the old executor does not count for the new one")
	}
	refuted.Call.Executor = moved.Call.Executor
	if restored.proven(routeKey(refuted)) || restored.covers(refuted) {
		t.Fatal("a refutation was lost with the old executor")
	}
}

// A MetricOmm route never passes a probe: its profit lasts until the next
// oracle update, about two seconds, and the probe runs a block later, so
// requiring a pass drops routes rivals then take. The pool trades WETH, WBTC
// and USDC only, so its leg counts as passed; the other legs and the funding
// still need their own.
func TestMetricOmmLegNeedsNoPassOfItsOwn(t *testing.T) {
	leg := func(kind domain.PoolKind, pool byte, in, out byte) execution.ExecutorStep {
		return execution.ExecutorStep{Pool: domain.PoolKey{Kind: kind, Address: domain.Address{pool}}, TokenIn: domain.Address{in}, TokenOut: domain.Address{out}}
	}
	route := func(steps ...execution.ExecutorStep) execution.ReactiveIntent {
		intent := preflightIntent()
		intent.Route.Funding, intent.Route.Asset, intent.Route.Steps = execution.FundingWallet, domain.Address{10}, steps
		return intent
	}
	p := &reactivePreflight{}
	p.pass(routeKey(route(leg(domain.PoolKindV3, 3, 10, 11), leg(domain.PoolKindV3, 5, 11, 10))), time.Now())
	through := route(leg(domain.PoolKindV3, 3, 10, 11), leg(domain.PoolKindMetricOmm, 8, 11, 10))
	if !p.proven(routeKey(through)) {
		t.Fatal("a MetricOmm leg kept a route of passed legs unproven")
	}
	if p.proven(routeKey(route(leg(domain.PoolKindV3, 4, 10, 11), leg(domain.PoolKindMetricOmm, 8, 11, 10)))) {
		t.Fatal("an unseen pool was proven by the MetricOmm leg beside it")
	}
	p.forget(through)
	if p.proven(routeKey(through)) {
		t.Fatal("a reverted MetricOmm route still counts as proven")
	}
}

// A two-hop route through a long-tail token, proven only leg by leg, can be
// quoted at $0.30 on $20 and bid high because every pool is held and a lost
// race is free. If both swaps run and the output is 3% short, the revert pays
// the tip on the whole gas, more than the quote was worth. A route counts as
// run only when it passed as a whole: simulated, traced or settled.
func TestOnlyAWholePassCountsAsARouteThatRan(t *testing.T) {
	leg := func(pool byte, in, out byte) execution.ExecutorStep {
		return execution.ExecutorStep{Pool: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{pool}}, TokenIn: domain.Address{in}, TokenOut: domain.Address{out}}
	}
	route := func(steps ...execution.ExecutorStep) execution.ReactiveIntent {
		intent := preflightIntent()
		intent.Route.Funding, intent.Route.Asset, intent.Route.Steps = execution.FundingV3FlashSwap, domain.Address{10}, steps
		return intent
	}
	pools := func(intent execution.ReactiveIntent) []domain.PoolKey { return planPools(intent.Route) }
	p := &reactivePreflight{}
	first, second := route(leg(3, 10, 11), leg(5, 11, 10)), route(leg(6, 10, 11), leg(4, 11, 10))
	p.pass(routeKey(first), time.Now())
	p.pass(routeKey(second), time.Now())
	composed := route(leg(6, 10, 11), leg(5, 11, 10))
	if !p.proven(routeKey(composed)) {
		t.Fatal("the composed route should be proven leg by leg")
	}
	if p.ran(execution.FundingV3FlashSwap, domain.Address{10}, pools(composed)) {
		t.Fatal("a route proven only leg by leg counts as run")
	}
	if !p.ran(execution.FundingV3FlashSwap, domain.Address{10}, pools(first)) {
		t.Fatal("a passed route does not count as run")
	}
	if p.ran(execution.FundingWallet, domain.Address{10}, pools(first)) || p.ran(execution.FundingV3FlashSwap, domain.Address{12}, pools(first)) {
		t.Fatal("another funding or start asset counts as the same route")
	}
	p.settled(composed, domain.Hash{1}, true, time.Now())
	if !p.ran(execution.FundingV3FlashSwap, domain.Address{10}, pools(composed)) {
		t.Fatal("a settled success does not count as run")
	}
	p.forget(first)
	if p.ran(execution.FundingV3FlashSwap, domain.Address{10}, pools(first)) {
		t.Fatal("a route that reverted still counts as run")
	}
}

// Blaming pools for probe reverts quarantines the busiest healthy pools, the
// ones rivals' arbitrage runs through most, including pools that settled a
// profitable send. A Fast route's probe runs at the written block, after the
// rivals took the opportunity, so its revert says nothing about the quote's
// pools. Only a call at the quote block counts, and a pool on a route that
// settled or passed its probe is proven.
func TestProbeRevertsAndSettledPoolsAreNotQuarantined(t *testing.T) {
	chain := domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: domain.ArbitrumOneChainID}
	busy := domain.PoolKey{Address: domain.Address{0xc6}}
	route := func(tail byte, source domain.StateSource) execution.ReactiveIntent {
		intent := preflightIntent()
		intent.Version.Source = source
		intent.Route.Steps = []execution.ExecutorStep{{Pool: busy}, {Pool: domain.PoolKey{Address: domain.Address{0x20, tail}}}}
		return intent
	}
	plainTrace := preflightServer(t, map[string]string{"debug_traceCall": `"result":{"type":"CALL","error":"execution reverted"}`})
	open := func() (*reactivePreflight, *poolrisk.Registry) {
		pools, err := poolrisk.Open("", chain)
		if err != nil {
			t.Fatal(err)
		}
		risks, err := tokenrisk.Open("", chain)
		if err != nil {
			t.Fatal(err)
		}
		return &reactivePreflight{pools: pools, risks: risks, trace: plainTrace, traceWait: time.Millisecond}, pools
	}
	p, pools := open()
	for i := range 3 {
		p.diagnoseRevert(t.Context(), route(byte(i+1), domain.SourceFast))
	}
	if pools.Check(busy) != nil {
		t.Fatal("probe reverts at the written block quarantined a pool")
	}
	p, pools = open()
	p.settled(route(9, domain.SourceFast), domain.Hash{1}, true, time.Now())
	for i := range 3 {
		p.diagnoseRevert(t.Context(), route(byte(i+1), domain.SourceStandard))
	}
	if pools.Check(busy) != nil {
		t.Fatal("a pool on a settled route was quarantined")
	}
}
