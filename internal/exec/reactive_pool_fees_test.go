package exec

import (
	"encoding/json"
	"errors"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A pre-upgrade intent remains a valid journal identity. Only new admission is
// prohibited; already submitted transactions must still settle after restart.
func legacyDynamicIntent(t *testing.T) (ReactiveIntent, DispatchLimits, *secretSigner) {
	t.Helper()
	i, limits, signer := dispatchFixture(t)
	key := i.Route.Steps[0].Pool
	key.Kind = domain.PoolKindSlipstream
	i.Route.Steps[0].Pool = key
	call, err := BuildExecutorCall(t.Context(), i.Route, i.Call.CallParameters)
	if err != nil {
		t.Fatal(err)
	}
	i.Call = call
	i.ID = domain.Hash{}
	raw, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	i.ID = keccak(raw)
	if err = i.Validate(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	return i, limits, signer
}
func TestDispatcherRejectsLegacyDynamicQuoteBeforeBroadcast(t *testing.T) {
	i, limits, signer := legacyDynamicIntent(t)
	network := &dispatchNetworkFixture{}
	dispatcher, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "dispatch.json"), limits, signer, network)
	if err != nil {
		t.Fatal(err)
	}
	defer dispatcher.Close()
	if _, err = dispatcher.Dispatch(t.Context(), i, func() bool { return true }); !errors.Is(err, ErrDispatchRejected) {
		t.Fatal("legacy dynamic quote admitted", err)
	}
	if network.calls != 0 || dispatcher.store.state.Pending != nil || dispatcher.store.state.Charged.Sign() != 0 {
		t.Fatal("rejected quote consumed a nonce, budget, or broadcast")
	}
}
func TestDynamicFeeRestrictionPreservesPendingJournalRecovery(t *testing.T) {
	i, limits, signer := legacyDynamicIntent(t)
	path := filepath.Join(t.TempDir(), "dispatch.json")
	network := &dispatchNetworkFixture{}
	dispatcher, err := OpenDispatcher(t.Context(), path, limits, signer, network)
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingTransaction{Intent: i, Nonce: 0, SubmittedMS: time.Now().UnixMilli()}
	unsigned := pendingEnvelope(pending)
	signed, err := signer.Sign(t.Context(), unsigned)
	if err != nil {
		t.Fatal(err)
	}
	pending.Hash = domain.Hash(signed.Hash())
	pending.Raw, err = signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	saved := dispatcher.store.state
	saved.Pending = &pending
	saved.Charged = new(big.Int).Set(i.Budget)
	if err = dispatcher.store.save(saved); err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
	network.pending = 1
	dispatcher, err = OpenDispatcher(t.Context(), path, limits, signer, network)
	if err != nil {
		t.Fatal("pending transaction became unrecoverable", err)
	}
	defer dispatcher.Close()
	quickFollowUps(dispatcher)
	network.addReceipt(paidReceipt(pending.Hash, 10, false, nil, 100))
	network.set(func(n *dispatchNetworkFixture) { n.latest = 1 })
	followAll(t, dispatcher, time.Now().Add(time.Minute))
	if dispatcher.Busy() || network.sends() != 0 {
		t.Fatal("old transaction did not settle without rebroadcast")
	}
}

func TestSlipstreamAdmissionRequiresBoundForFinalSignedTip(t *testing.T) {
	i, _, _ := legacyDynamicIntent(t)
	i.PoolFees = []ReactivePoolFee{{Pool: i.Route.Steps[0].Pool, FeePips: 400, BaseFee: new(big.Int), PriorityFee: new(big.Int).Set(i.Call.TipCap)}}
	sealed, err := SealReactiveIntent(t.Context(), i)
	if err != nil {
		t.Fatal(err)
	}
	if err = sealed.Validate(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	i.PoolFees[0].PriorityFee.Add(i.PoolFees[0].PriorityFee, big.NewInt(1))
	if _, err = SealReactiveIntent(t.Context(), i); err == nil {
		t.Fatal("bound for a different bid admitted")
	}
	i.PoolFees[0].PriorityFee.Set(i.Call.TipCap)
	i.PoolFees[0].BaseFee.SetInt64(1)
	if _, err = SealReactiveIntent(t.Context(), i); err == nil {
		t.Fatal("assumed base-fee threshold admitted")
	}
}
