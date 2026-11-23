package exec

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// dispatchNetworkFixture is shared with follow-up tasks, which run on their
// own goroutines; tests that start them use set and sends.
type dispatchNetworkFixture struct {
	mu              sync.Mutex
	pending, latest uint64
	calls           int
	fail            bool
	// rejectConditions answers every conditional send with -32003.
	rejectConditions bool
	conditions       []*TransactionConditions
	receipts         map[domain.Hash]*rpc.TransactionReceipt
	hidden           int // receipt reads that answer nil before receipts show
	reads            int
}

func (n *dispatchNetworkFixture) PendingNonce(context.Context, domain.Address) (uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pending, nil
}
func (n *dispatchNetworkFixture) ConfirmedNonce(context.Context, domain.Address) (uint64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.latest, nil
}
func (n *dispatchNetworkFixture) Send(_ context.Context, raw []byte, conditions *TransactionConditions) (domain.Hash, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	n.conditions = append(n.conditions, conditions)
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return domain.Hash{}, err
	}
	if conditions != nil && n.rejectConditions {
		return domain.Hash{}, fmt.Errorf("%w: condition not met", ErrConditionsRejected)
	}
	n.pending = max(n.pending, tx.Nonce()+1)
	if n.fail {
		return domain.Hash{}, errors.New("response lost")
	}
	return domain.Hash(tx.Hash()), nil
}
func (n *dispatchNetworkFixture) Receipt(_ context.Context, hash domain.Hash) (*rpc.TransactionReceipt, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reads++
	if n.hidden > 0 {
		n.hidden--
		return nil, nil
	}
	return n.receipts[hash], nil
}
func (n *dispatchNetworkFixture) Diagnose(context.Context, ReactiveIntent, *Settlement) string {
	return ""
}
func (n *dispatchNetworkFixture) set(update func(*dispatchNetworkFixture)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	update(n)
}
func (n *dispatchNetworkFixture) sends() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}
func (n *dispatchNetworkFixture) receiptReads() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reads
}
func (n *dispatchNetworkFixture) addReceipt(receipt *rpc.TransactionReceipt) {
	n.set(func(n *dispatchNetworkFixture) {
		if n.receipts == nil {
			n.receipts = map[domain.Hash]*rpc.TransactionReceipt{}
		}
		n.receipts[receipt.Hash] = receipt
	})
}

// paidReceipt is a canonical receipt in block number with the given total fee.
func paidReceipt(hash domain.Hash, number uint64, success bool, logs []rpc.Log, fee int64) *rpc.TransactionReceipt {
	return &rpc.TransactionReceipt{Hash: hash, Block: domain.BlockRef{Number: number, Hash: domain.Hash{byte(number), 0xb}}, Success: success, Logs: logs,
		Fees: &domain.ReceiptFees{Family: domain.FamilyOPStack, GasUsed: 15000, Total: big.NewInt(fee)}}
}

// followAll runs one Watch and applies every follow-up it starts.
func followAll(t *testing.T, d *Dispatcher, now time.Time) []SettlementUpdate {
	t.Helper()
	updates, err := d.Watch(now)
	if err != nil {
		t.Fatal(err)
	}
	for runningTasks(d) > 0 {
		select {
		case f := <-d.Followups():
			more, err := d.ApplyFollowup(f, now)
			if err != nil {
				t.Fatal(err)
			}
			updates = append(updates, more...)
		case <-time.After(5 * time.Second):
			t.Fatal("follow-up did not finish")
		}
	}
	return updates
}

func runningTasks(d *Dispatcher) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.running)
}

// quickFollowUps makes receipt follow-ups read at once and recheck without waiting.
func quickFollowUps(d *Dispatcher) {
	d.timing.feeAttempts = []time.Duration{0, time.Millisecond}
	d.timing.recheck = time.Millisecond
}

func dispatchFixture(t *testing.T) (ReactiveIntent, DispatchLimits, *secretSigner) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := &secretSigner{key: key, address: domain.Address(crypto.PubkeyToAddress(key.PublicKey))}
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	route := ExecutorPlan{Funding: FundingWallet, Asset: a, Amount: big.NewInt(100), MaxLoanFee: new(big.Int), MinProfit: big.NewInt(100001), Deadline: uint64(time.Now().Unix() + 2), Steps: []ExecutorStep{{Pool: p, TokenIn: a, TokenOut: b, Fee: 3000, MinAmountOut: big.NewInt(1)}, {Pool: q, TokenIn: b, TokenOut: a, Fee: 3000, MinAmountOut: big.NewInt(1)}}}
	params := CallParameters{Sender: signer.Address(), Executor: domain.Address{9}, CodeHash: domain.Hash{9}, GasLimit: 21000, FeeCap: big.NewInt(2), TipCap: big.NewInt(1)}
	call, err := BuildExecutorCall(t.Context(), route, params)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	intent, err := SealReactiveIntent(t.Context(), ReactiveIntent{QuotedGross: big.NewInt(100010), QuoteReserve: new(big.Int), Chain: chain, Version: domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{2}}, Route: route, Call: call, Budget: big.NewInt(100000), ExpectedNet: big.NewInt(10), MinimumNet: big.NewInt(1), Conversion: FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, CreatedMS: now, ExpiresMS: now + 500, ContextExpiresMS: now + 1000})
	if err != nil {
		t.Fatal(err)
	}
	limits := DispatchLimits{Chain: chain, Sender: signer.Address(), Executor: params.Executor, CodeHash: params.CodeHash, PerTransaction: big.NewInt(100000), PerDay: big.NewInt(100000), Assets: map[domain.Address]*big.Int{a: big.NewInt(1000), b: big.NewInt(1000)}, StopFile: filepath.Join(t.TempDir(), "STOP"), TokenCheck: func(domain.Address) error { return nil }}
	return intent, limits, signer
}

func TestDispatchAmbiguousSendSurvivesRestartAndChargesFailedReceipt(t *testing.T) {
	i, l, s := dispatchFixture(t)
	network := &dispatchNetworkFixture{fail: true}
	path := filepath.Join(t.TempDir(), "dispatch.json")
	d, err := OpenDispatcher(t.Context(), path, l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err == nil || hash == (domain.Hash{}) || network.calls != 1 {
		t.Fatal(hash, err)
	}
	if _, err = d.Dispatch(t.Context(), i, func() bool { return true }); !errors.Is(err, ErrDispatchBusy) {
		t.Fatal(err)
	}
	d.Close()
	d, err = OpenDispatcher(t.Context(), path, l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	quickFollowUps(d)
	if !d.Busy() {
		t.Fatal("unknown submission lost")
	}
	// Never seen in the feed: after the bound, one nonce read finds it mined.
	network.set(func(n *dispatchNetworkFixture) { n.latest = 1 })
	network.addReceipt(paidReceipt(hash, 10, false, nil, 100))
	followAll(t, d, time.Now().Add(31*time.Second))
	if d.Busy() || d.store.state.NextNonce != 1 || d.store.state.Charged.Int64() != 100 || len(d.store.state.Included) != 0 {
		t.Fatal("ambiguous send not settled from its receipt", d.store.state.Charged, d.store.state.HaltedReason)
	}
	i.ID = domain.Hash{}
	i.CreatedMS++
	i, err = SealReactiveIntent(t.Context(), i)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Dispatch(t.Context(), i, func() bool { return true }); !errors.Is(err, ErrDispatchRejected) {
		t.Fatal(err)
	}
	if network.sends() != 1 {
		t.Fatal("daily budget allowed another send")
	}
}

func TestDispatchRejectsUntrackedPendingNonceAndStaleIntent(t *testing.T) {
	i, l, s := dispatchFixture(t)
	path := filepath.Join(t.TempDir(), "dispatch.json")
	if d, err := OpenDispatcher(t.Context(), path, l, s, &dispatchNetworkFixture{pending: 1}); err == nil {
		d.Close()
		t.Fatal("untracked pending accepted")
	}
	network := &dispatchNetworkFixture{}
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "dispatch.json"), l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err = d.Dispatch(t.Context(), i, func() bool { return false }); !errors.Is(err, ErrDispatchRejected) {
		t.Fatal(err)
	}
	i.Call.TipCap.Add(i.Call.TipCap, big.NewInt(1))
	if _, err = d.Dispatch(t.Context(), i, func() bool { return true }); !errors.Is(err, ErrDispatchRejected) {
		t.Fatal(err)
	}
	if network.calls != 0 {
		t.Fatal("invalid intent sent")
	}
}

func TestReactiveMalformedEnvelopeReturnsError(t *testing.T) {
	i, _, _ := dispatchFixture(t)
	i.Call.FeeCap = nil
	if _, err := SealReactiveIntent(t.Context(), i); err == nil {
		t.Fatal("missing fee accepted")
	}
}

func TestReplacementRetainsOriginalHashWhenOriginalMines(t *testing.T) {
	i, l, s := dispatchFixture(t)
	l.PerDay = big.NewInt(300000)
	l.PerTransaction = big.NewInt(200000)
	network := &dispatchNetworkFixture{}
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "state.json"), l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	original, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	replacement := i
	replacement.Call.FeeCap = big.NewInt(3)
	replacement.Call.TipCap = big.NewInt(2)
	replacement.Budget = big.NewInt(110000)
	replacement.QuotedGross = big.NewInt(110020)
	replacement.ExpectedNet = big.NewInt(20)
	replacement.Route.MinProfit = big.NewInt(110001)
	replacement.Call, err = BuildExecutorCall(t.Context(), replacement.Route, replacement.Call.CallParameters)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err = SealReactiveIntent(t.Context(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Replace(t.Context(), replacement, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if len(d.store.state.Pending.Previous) != 1 || d.store.state.Charged.Int64() != 110000 {
		t.Fatal("replacement lost variant or double charged nonce")
	}
	observed := d.ObserveExecution(original, true, observationLogs(i), ExecutionPosition{BlockNumber: 10})
	updates, err := d.SettleObserved(observed, time.Now())
	if err != nil || len(updates) != 1 || updates[0].Settlement.Hash != original || updates[0].Settlement.IntentID != i.ID {
		t.Fatal("original variant not booked", updates, err)
	}
	if d.store.state.NextNonce != 1 || d.Busy() {
		t.Fatal("original inclusion did not settle replacement lane")
	}
}

// The journal is written while the send is in flight. A failed write leaves the
// broadcast uncertain, poisons the store, and refuses every later send before
// it reaches the network; restart reconciliation then sees the nonce moved.
func TestDispatchJournalFailurePoisonsOwnerAndBlocksLaterSends(t *testing.T) {
	i, l, s := dispatchFixture(t)
	network := &dispatchNetworkFixture{}
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "state.json"), l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Rename cannot replace a nonempty directory, so the journal publish fails.
	d.store.path = t.TempDir()
	if _, err = d.Dispatch(t.Context(), i, func() bool { return true }); !errors.Is(err, ErrSubmissionUnknown) {
		t.Fatal("journal failure during send not reported as an uncertain submission", err)
	}
	if !d.store.failed || d.Pending() == nil {
		t.Fatal("failed journal did not poison the owner or lost the in-flight nonce")
	}
	sent := network.calls
	if _, err = d.Dispatch(t.Context(), i, func() bool { return true }); err == nil || network.calls != sent {
		t.Fatal("send allowed after the journal failed")
	}
}

func TestFeeOverrunIsRecordedAndHaltsFurtherTrading(t *testing.T) {
	i, l, s := dispatchFixture(t)
	network := &dispatchNetworkFixture{}
	d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "state.json"), l, s, network)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	hash, err := d.Dispatch(t.Context(), i, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	d.ObserveCanonical(domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, []CanonicalReceipt{{Hash: hash, Fees: &domain.ReceiptFees{Total: big.NewInt(100001)}}})
	updates := followAll(t, d, time.Now())
	if len(updates) != 1 || !updates[0].FeesBooked {
		t.Fatal(updates)
	}
	if d.store.state.Charged.Int64() != 100001 || d.store.state.HaltedReason == "" || !d.Busy() {
		t.Fatal("overrun lost or trading continued")
	}
	if _, err = d.Dispatch(t.Context(), another(t, i), func() bool { return true }); !errors.Is(err, ErrDispatchRejected) {
		t.Fatal(err)
	}
}

func TestDispatcherRequiresTokenCheck(t *testing.T) {
	_, limits, signer := dispatchFixture(t)
	limits.TokenCheck = nil
	if _, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "dispatch.json"), limits, signer, &dispatchNetworkFixture{}); err == nil {
		t.Fatal("dispatcher opened without a token shortfall check")
	}
}
