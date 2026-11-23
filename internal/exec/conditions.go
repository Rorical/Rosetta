package exec

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"

	"github.com/Rorical/Rosetta/internal/domain"
)

// TransactionConditions are eth_sendRawTransactionConditional's options. The
// sequencer drops the transaction without charge or nonce when a slot differs
// from its value or the block timestamp passes TimestampMax. Nitro checks them
// on admission, also against a state at least 2 s old, and again at the
// transaction's execution position.
type TransactionConditions struct {
	Accounts     []AccountConditions
	TimestampMax uint64
}

// AccountConditions are expected storage values of one account.
type AccountConditions struct {
	Address domain.Address
	Slots   []SlotCondition
}

type SlotCondition struct {
	Slot, Value domain.Hash
}

// HotConditions hold the executor's slot 0 at unpaused and configVersion until
// deadline. ExecutorAdmin packs paused (byte offset 0) and configVersion (offset
// 1) into slot 0 and every owner setter moves configVersion, so a pause or any
// reconfiguration voids the transaction; executeV3Hot reads neither.
func HotConditions(executor domain.Address, configVersion, deadline uint64) *TransactionConditions {
	var word domain.Hash
	binary.BigEndian.PutUint64(word[23:31], configVersion)
	return &TransactionConditions{
		Accounts:     []AccountConditions{{Address: executor, Slots: []SlotCondition{{Value: word}}}},
		TimestampMax: deadline,
	}
}

// Hold adds slots to account's conditions. V4 pools share the PoolManager,
// so one account can hold several pools' slots; the RPC takes one slot map
// per account.
func (c *TransactionConditions) Hold(account domain.Address, slots []SlotCondition) {
	for i := range c.Accounts {
		if c.Accounts[i].Address != account {
			continue
		}
		for _, slot := range slots {
			if !slices.Contains(c.Accounts[i].Slots, slot) {
				c.Accounts[i].Slots = append(c.Accounts[i].Slots, slot)
			}
		}
		return
	}
	c.Accounts = append(c.Accounts, AccountConditions{Address: account, Slots: slices.Clone(slots)})
}

func (c *TransactionConditions) holds(account domain.Address) bool {
	for _, a := range c.Accounts {
		if a.Address == account && len(a.Slots) > 0 {
			return true
		}
	}
	return false
}

// ClaimEpochSlot is HotV3Executor.claimEpoch's storage slot, after
// ExecutorAdmin's slot 0 (paused, configVersion) and slot 1 (operators).
var ClaimEpochSlot = domain.Hash{31: 2}

// WithClaim makes a hot intent claim its opportunity at epoch: its route
// raises claimEpoch on success and its conditions hold the current value. The intent is resealed.
func WithClaim(ctx context.Context, i ReactiveIntent, epoch uint64) (ReactiveIntent, error) {
	if !i.Route.Hot || i.Conditions == nil {
		return ReactiveIntent{}, fmt.Errorf("only a conditional hot route claims an opportunity")
	}
	i.Route.Claim = true
	call, err := BuildExecutorCall(ctx, i.Route, i.Call.CallParameters)
	if err != nil {
		return ReactiveIntent{}, err
	}
	conditions := &TransactionConditions{TimestampMax: i.Conditions.TimestampMax}
	for _, account := range i.Conditions.Accounts {
		conditions.Hold(account.Address, account.Slots)
	}
	var value domain.Hash
	binary.BigEndian.PutUint64(value[24:], epoch)
	conditions.Hold(i.Call.Executor, []SlotCondition{{Slot: ClaimEpochSlot, Value: value}})
	i.ID, i.Call, i.Conditions = domain.Hash{}, call, conditions
	return SealReactiveIntent(ctx, i)
}

func (c *TransactionConditions) holdsSlot(account domain.Address, slot domain.Hash) bool {
	for _, a := range c.Accounts {
		if a.Address == account && slices.ContainsFunc(a.Slots, func(s SlotCondition) bool { return s.Slot == slot }) {
			return true
		}
	}
	return false
}

// options is the JSON-RPC parameter; Nitro decodes the timestamp as a hex
// quantity.
func (c *TransactionConditions) options() map[string]any {
	accounts := make(map[string]map[string]string, len(c.Accounts))
	for _, account := range c.Accounts {
		slots := make(map[string]string, len(account.Slots))
		for _, slot := range account.Slots {
			slots[slot.Slot.Hex()] = slot.Value.Hex()
		}
		accounts[account.Address.Hex()] = slots
	}
	return map[string]any{"knownAccounts": accounts, "timestampMax": "0x" + strconv.FormatUint(c.TimestampMax, 16)}
}
