package exec

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A sweep and an unwrap take the arbitrage nonce lane in turn: the journal
// holds the maintenance across a restart, an arbitrage cannot replace or
// cancel it, its receipt books minus the paid fee with no arbitrage outcome,
// and the fee counts against the day's budget.
func TestMaintenanceSharesTheNonceLane(t *testing.T) {
	intent, limits, signer := dispatchFixture(t)
	wrapped := intent.Route.Asset
	limits.Wrapped = wrapped
	limits.PerDay = big.NewInt(1_000_000)
	network := &dispatchNetworkFixture{}
	path := filepath.Join(t.TempDir(), "dispatch.json")
	d, err := OpenDispatcher(t.Context(), path, limits, signer, network)
	if err != nil {
		t.Fatal(err)
	}
	quickFollowUps(d)
	current := func() bool { return true }
	now := time.Now()
	sweep := NewMaintenance(MaintenanceSweep, wrapped, big.NewInt(50), 60000, big.NewInt(1), new(big.Int), now)
	hash, err := d.Maintain(t.Context(), sweep, current)
	if err != nil {
		t.Fatal(err)
	}
	var sent types.Transaction
	if err := sent.UnmarshalBinary(d.store.state.Pending.Raw); err != nil {
		t.Fatal(err)
	}
	_, wantData := sweep.Call(limits.Executor)
	if domain.Address(*sent.To()) != limits.Executor || !bytes.Equal(sent.Data(), wantData) || sent.Nonce() != 0 || sent.Value().Sign() != 0 || len(wantData) != 68 {
		t.Fatal("sweep envelope", sent.To(), sent.Data())
	}
	if _, err := d.Replace(t.Context(), intent, current); !errors.Is(err, ErrDispatchRejected) {
		t.Fatal("an arbitrage replaced a maintenance transaction", err)
	}
	if _, err := d.Dispatch(t.Context(), intent, current); !errors.Is(err, ErrDispatchBusy) {
		t.Fatal("an arbitrage took the busy lane", err)
	}
	d.Close()

	// An edited journal cannot turn the maintenance into another call.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	pending := state["Pending"].(map[string]any)
	pending["Maintenance"].(map[string]any)["Amount"] = 51
	edited, _ := json.Marshal(state)
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDispatcher(t.Context(), path, limits, signer, network); err == nil {
		t.Fatal("edited maintenance accepted")
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	d, err = OpenDispatcher(t.Context(), path, limits, signer, network)
	if err != nil {
		t.Fatal("maintenance did not survive a restart:", err)
	}
	defer d.Close()
	quickFollowUps(d)

	network.set(func(n *dispatchNetworkFixture) { n.latest = 1 })
	network.addReceipt(paidReceipt(hash, 10, true, nil, 70))
	updates := followAll(t, d, now.Add(31*time.Second))
	var booked *Settlement
	for _, u := range updates {
		if u.Settlement != nil && u.Settlement.Hash == hash {
			booked = u.Settlement
		}
	}
	if booked == nil || booked.Maintenance != MaintenanceSweep || booked.Profit.Sign() != 0 || booked.Net == nil || booked.Net.Int64() != -70 || d.Busy() || d.store.state.Charged.Int64() != 70 {
		t.Fatalf("sweep settlement %+v charged %v", booked, d.store.state.Charged)
	}

	unwrap := NewMaintenance(MaintenanceUnwrap, wrapped, big.NewInt(50), 60000, big.NewInt(1), new(big.Int), now.Add(time.Second))
	if _, err := d.Maintain(t.Context(), unwrap, current); err != nil {
		t.Fatal(err)
	}
	if err := sent.UnmarshalBinary(d.store.state.Pending.Raw); err != nil {
		t.Fatal(err)
	}
	_, wantData = unwrap.Call(limits.Executor)
	if domain.Address(*sent.To()) != wrapped || !bytes.Equal(sent.Data(), wantData) || sent.Nonce() != 1 || len(wantData) != 36 {
		t.Fatal("unwrap envelope", sent.To(), sent.Data())
	}
}

// Only the two fixed calls validate: a sweep of a start asset, an unwrap of
// the configured wrapped native.
func TestMaintenanceValidation(t *testing.T) {
	intent, limits, _ := dispatchFixture(t)
	wrapped, other := intent.Route.Asset, domain.Address{7}
	limits.Wrapped = wrapped
	now := time.Now()
	valid := func(kind string, token domain.Address) error {
		return NewMaintenance(kind, token, big.NewInt(1), 60000, big.NewInt(1), new(big.Int), now).validate(limits)
	}
	if valid(MaintenanceSweep, wrapped) != nil || valid(MaintenanceUnwrap, wrapped) != nil {
		t.Fatal("valid maintenance rejected")
	}
	if valid(MaintenanceSweep, other) == nil || valid(MaintenanceUnwrap, intent.Route.Steps[0].TokenOut) == nil || valid("transfer", wrapped) == nil {
		t.Fatal("arbitrary maintenance accepted")
	}
	limits.Wrapped = domain.Address{}
	if valid(MaintenanceUnwrap, wrapped) == nil {
		t.Fatal("unwrap allowed without a configured wrapped native")
	}
	short := NewMaintenance(MaintenanceSweep, wrapped, big.NewInt(1), 60000, big.NewInt(1), new(big.Int), now)
	short.Budget = big.NewInt(1)
	if short.validate(limits) == nil {
		t.Fatal("budget below the gas envelope accepted")
	}
}
