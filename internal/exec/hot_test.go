package exec

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// hotPlan: asset 0x01 -> 0x02 on a Uniswap V3 pool, back on a Camelot V3 pool.
func hotPlan() ExecutorPlan {
	a, b := domain.Address{1}, domain.Address{2}
	return ExecutorPlan{Funding: FundingV3FlashSwap, Asset: a, Amount: big.NewInt(1000), MaxLoanFee: new(big.Int), MinProfit: big.NewInt(100), Deadline: 1_900_000_000,
		Steps: []ExecutorStep{
			{Pool: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{0xaa}}, TokenIn: a, TokenOut: b, MinAmountOut: big.NewInt(1)},
			{Pool: domain.PoolKey{Kind: domain.PoolKindCamelotV3, Address: domain.Address{0xbb}}, TokenIn: b, TokenOut: a, MinAmountOut: big.NewInt(1), PriceBound: big.NewInt(0x1234)},
		}}
}

// The layout HotV3Executor.loadRoute and checkBounds decode
// (contracts/test/HotTestRoute.sol builds the same bytes).
func TestHotRouteEncoding(t *testing.T) {
	plan := hotPlan()
	plan.Hot = true
	encoded, err := EncodeHotV3Plan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	word := func(fill byte, n int) string { return strings.Repeat(hex.EncodeToString([]byte{fill}), n) }
	address := func(first byte) string { return hex.EncodeToString([]byte{first}) + word(0, 19) }
	payload := address(1) + word(0, 14) + "03e8" + word(0, 15) + "64" + "02" +
		address(0xaa) + address(2) + address(0xbb) + address(1) +
		"81" + word(0, 18) + "1234"
	selector := keccak([]byte("executeV3Hot(bytes)"))
	want := hex.EncodeToString(selector[:4]) + word(0, 31) + "20" + word(0, 31) + "9a" + payload + word(0, 6)
	if got := hex.EncodeToString(encoded); got != want {
		t.Fatalf("hot calldata\n got %s\nwant %s", got, want)
	}
	call, err := BuildExecutorCall(t.Context(), plan, CallParameters{Sender: domain.Address{7}, Executor: domain.Address{8}, CodeHash: domain.Hash{9}, GasLimit: 300_000, FeeCap: big.NewInt(2), TipCap: big.NewInt(1)})
	if err != nil || hex.EncodeToString(call.Data) != want {
		t.Fatal("a hot plan did not build executeV3Hot calldata", err)
	}
}

func TestHotRouteSupported(t *testing.T) {
	if !HotRouteSupported(hotPlan()) {
		t.Fatal("V3 and Camelot flash-swap route rejected")
	}
	for name, change := range map[string]func(*ExecutorPlan){
		"wallet funding":     func(p *ExecutorPlan) { p.Funding = FundingWallet },
		"V2 leg":             func(p *ExecutorPlan) { p.Steps[1].Pool.Kind = domain.PoolKindV2 },
		"Solidly V3 leg":     func(p *ExecutorPlan) { p.Steps[1].Pool.Kind = domain.PoolKindSolidlyV3 },
		"amount over 128":    func(p *ExecutorPlan) { p.Amount = new(big.Int).Lsh(big.NewInt(1), 128) },
		"cumulative check":   func(p *ExecutorPlan) { p.MinRate = big.NewInt(1) },
		"price limit":        func(p *ExecutorPlan) { p.Steps[0].SqrtPriceLimitX96 = big.NewInt(5) },
		"min profit too big": func(p *ExecutorPlan) { p.MinProfit = new(big.Int).Lsh(big.NewInt(1), 130) },
	} {
		plan := hotPlan()
		change(&plan)
		if HotRouteSupported(plan) {
			t.Error(name, "accepted")
		}
	}
}

// hotIntent seals a hot route as dispatchFixture seals a packed one, with
// the caller's deadline: one clock reading for route and conditions.
func hotIntent(t *testing.T, conditions *TransactionConditions, deadline uint64) (ReactiveIntent, error) {
	t.Helper()
	i, _, _ := dispatchFixture(t)
	route := hotPlan()
	route.Hot = true
	route.MinProfit = big.NewInt(100001)
	route.Deadline = deadline
	call, err := BuildExecutorCall(t.Context(), route, i.Call.CallParameters)
	if err != nil {
		t.Fatal(err)
	}
	i.ID, i.Route, i.Call, i.Conditions = domain.Hash{}, route, call, conditions
	return SealReactiveIntent(t.Context(), i)
}

func TestHotIntentRequiresExecutorConditions(t *testing.T) {
	executor := domain.Address{9}
	deadline := uint64(time.Now().Unix() + 2)
	// hotPlan bounds its Camelot leg only; a condition holds the first pool.
	holding := func(c *TransactionConditions) *TransactionConditions {
		c.Accounts = append(c.Accounts, AccountConditions{Address: domain.Address{0xaa}, Slots: []SlotCondition{{Value: domain.Hash{1}}}})
		return c
	}
	if _, err := hotIntent(t, holding(HotConditions(executor, 3, deadline)), deadline); err != nil {
		t.Fatal(err)
	}
	for name, conditions := range map[string]*TransactionConditions{
		"none":              nil,
		"another account":   holding(HotConditions(domain.Address{5}, 3, deadline)),
		"past the deadline": holding(HotConditions(executor, 3, deadline+1)),
		"an unheld leg":     HotConditions(executor, 3, deadline),
	} {
		if _, err := hotIntent(t, conditions, deadline); err == nil {
			t.Error(name, "accepted")
		}
	}
}

func TestHotConditionsHoldUnpausedConfigVersion(t *testing.T) {
	options := HotConditions(domain.Address{9}, 0x0102, 1234).options()
	encoded, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	// ExecutorAdmin slot 0: paused at byte offset 0, configVersion from offset 1.
	want := `{"knownAccounts":{"` + domain.Address{9}.Hex() + `":{"` + domain.Hash{}.Hex() + `":"0x` + strings.Repeat("00", 29) + `010200"}},"timestampMax":"0x4d2"}`
	if string(encoded) != want {
		t.Fatalf("options\n got %s\nwant %s", encoded, want)
	}
}

func TestConditionalSendUsesTheSequencerAndReportsRejection(t *testing.T) {
	var method string
	var params []json.RawMessage
	reply := `{"jsonrpc":"2.0","id":1,"result":"` + domain.Hash{0xab}.Hex() + `"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		json.Unmarshal(body, &request)
		method, params = request.Method, request.Params
		w.Write([]byte(reply))
	}))
	defer server.Close()
	ack := `{"jsonrpc":"2.0","id":1,"result":"` + domain.Hash{0xcd}.Hex() + `"}`
	n := RPCDispatchNetwork{Submit: rpc.NewClient(server.URL, rpc.Options{Timeout: time.Second}), Fanout: []SubmitPath{{Name: "other", Client: sendServer(t, 0, ack)}}}
	got, err := n.Send(t.Context(), []byte{1}, HotConditions(domain.Address{9}, 1, 5))
	if err != nil || got != (domain.Hash{0xab}) || method != "eth_sendRawTransactionConditional" || len(params) != 2 || string(params[0]) != `"0x01"` || !strings.Contains(string(params[1]), `"timestampMax":"0x5"`) {
		t.Fatal("conditional send", got, err, method, params)
	}
	reply = `{"jsonrpc":"2.0","id":1,"error":{"code":-32003,"message":"condition not met"}}`
	if _, err := n.Send(t.Context(), []byte{1}, HotConditions(domain.Address{9}, 1, 5)); !errors.Is(err, ErrConditionsRejected) {
		t.Fatal("-32003 not reported as a rejection", err)
	}
	reply = `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"nonce too low"}}`
	if _, err := n.Send(t.Context(), []byte{1}, HotConditions(domain.Address{9}, 1, 5)); err == nil || errors.Is(err, ErrConditionsRejected) {
		t.Fatal("another error reported as a rejection", err)
	}
}

// A dropped conditional send used neither its nonce nor its budget; another
// send may take the lane at once.
func TestRejectedConditionalSendReleasesTheLane(t *testing.T) {
	i, l, s := dispatchFixture(t)
	i.ID = domain.Hash{}
	i.Conditions = HotConditions(l.Executor, 1, i.Route.Deadline)
	i, err := SealReactiveIntent(t.Context(), i)
	if err != nil {
		t.Fatal(err)
	}
	network := &dispatchNetworkFixture{rejectConditions: true}
	path := filepath.Join(t.TempDir(), "dispatch.json")
	d, err := OpenDispatcher(t.Context(), path, l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if !errors.Is(err, ErrDispatchRejected) || !errors.Is(err, ErrConditionsRejected) || hash != (domain.Hash{}) {
		t.Fatal("rejection", hash, err)
	}
	if d.Busy() || d.Pending() != nil || d.store.state.NextNonce != 0 || d.store.state.Charged.Sign() != 0 {
		t.Fatal("a rejected send kept its nonce or budget", d.store.state.Charged)
	}
	network.set(func(n *dispatchNetworkFixture) { n.rejectConditions = false })
	if _, err := d.Dispatch(t.Context(), i, func() bool { return true }); err != nil || network.calls != 2 || network.conditions[1] == nil {
		t.Fatal("the lane stayed closed after a rejection", err)
	}
	replacement := i
	replacement.ID = domain.Hash{}
	replacement.CreatedMS++
	if replacement, err = SealReactiveIntent(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Replace(t.Context(), replacement, func() bool { return true }); !errors.Is(err, ErrDispatchRejected) || !strings.Contains(err.Error(), "conditional") {
		t.Fatal("a conditional transaction was replaced", err)
	}
}

func TestForSenderResealsForAnotherLane(t *testing.T) {
	i, _, _ := dispatchFixture(t)
	if _, err := ForSender(t.Context(), i, domain.Address{7}); err == nil {
		t.Fatal("a wallet-funded route changed its sender")
	}
	held := HotConditions(domain.Address{9}, 3, i.Route.Deadline)
	held.Accounts = append(held.Accounts, AccountConditions{Address: domain.Address{0xaa}, Slots: []SlotCondition{{Value: domain.Hash{1}}}})
	hot, err := hotIntent(t, held, i.Route.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := ForSender(t.Context(), hot, domain.Address{7})
	if err != nil || moved.Call.Sender != (domain.Address{7}) || moved.ID == hot.ID || moved.Validate(t.Context(), time.UnixMilli(moved.CreatedMS)) != nil {
		t.Fatal("intent not resealed for the other sender", err)
	}
	if same, err := ForSender(t.Context(), hot, hot.Call.Sender); err != nil || same.ID != hot.ID {
		t.Fatal("own sender resealed", err)
	}
}

// V4 pools share the PoolManager: their slots merge under one account, which
// the RPC options take as one slot map.
func TestHoldMergesSlotsOfOneAccount(t *testing.T) {
	c := &TransactionConditions{}
	manager := domain.Address{0x36}
	c.Hold(manager, []SlotCondition{{Slot: domain.Hash{1}, Value: domain.Hash{2}}})
	c.Hold(manager, []SlotCondition{{Slot: domain.Hash{3}, Value: domain.Hash{4}}, {Slot: domain.Hash{1}, Value: domain.Hash{2}}})
	c.Hold(domain.Address{5}, []SlotCondition{{Value: domain.Hash{6}}})
	if len(c.Accounts) != 2 || len(c.Accounts[0].Slots) != 2 || !c.holds(manager) {
		t.Fatalf("%+v", c.Accounts)
	}
}

func TestClaimSetsTheStepsBitAndHoldsTheEpoch(t *testing.T) {
	executor := domain.Address{9}
	deadline := uint64(time.Now().Unix() + 2)
	conditions := HotConditions(executor, 3, deadline)
	conditions.Hold(domain.Address{0xaa}, []SlotCondition{{Value: domain.Hash{1}}})
	hot, err := hotIntent(t, conditions, deadline)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := WithClaim(t.Context(), hot, 5)
	if err != nil {
		t.Fatal(err)
	}
	// Byte 68 + 52 is the steps byte after the selector and ABI head.
	if !claimed.Route.Claim || claimed.Call.Data[68+52] != 0x82 || hot.Call.Data[68+52] != 0x02 || !claimed.Conditions.holdsSlot(executor, ClaimEpochSlot) || len(hot.Conditions.Accounts) != 2 {
		t.Fatal("claim not encoded or not held", claimed.Call.Data[68+52])
	}
	var five domain.Hash
	five[31] = 5
	if claimed.Conditions.Accounts[0].Slots[1] != (SlotCondition{Slot: ClaimEpochSlot, Value: five}) {
		t.Fatal("claim epoch value", claimed.Conditions.Accounts[0].Slots)
	}
	unheld := claimed
	unheld.ID, unheld.Conditions = domain.Hash{}, conditions
	if _, err := SealReactiveIntent(t.Context(), unheld); err == nil {
		t.Fatal("a claiming route sealed without its epoch condition")
	}
	if _, err := BuildExecutorCall(t.Context(), ExecutorPlan{Claim: true}, hot.Call.CallParameters); err == nil {
		t.Fatal("a packed route claimed")
	}
}
