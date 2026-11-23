package exec

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Pending returns an immutable snapshot without waiting for signing or RPC.
func (d *Dispatcher) Pending() *PendingTransaction { return d.pending.Load() }
func (d *Dispatcher) Included() bool {
	p := d.Pending()
	return p != nil && d.observedNonce.Load() == p.Nonce+1
}

// ObserveIncluded stops updates as soon as any signed variant appears in an
// accepted prefix or canonical block. The next send then releases the nonce.
func (d *Dispatcher) ObserveIncluded(hashes []domain.Hash) {
	p := d.Pending()
	if p == nil {
		return
	}
	for _, h := range hashes {
		if h == p.Hash {
			d.observeNonce(p.Nonce)
			return
		}
		for _, old := range p.Previous {
			if h == old.Hash {
				d.observeNonce(p.Nonce)
				return
			}
		}
	}
}

func (d *Dispatcher) observeNonce(nonce uint64) {
	for {
		old := d.observedNonce.Load()
		if old >= nonce+1 || d.observedNonce.CompareAndSwap(old, nonce+1) {
			return
		}
	}
}

func pendingEnvelope(p PendingTransaction) *types.Transaction {
	if p.Maintenance != nil {
		return maintenanceEnvelope(p)
	}
	i := p.Intent
	to := common.Address(i.Call.Executor)
	gas, fee, tip, value, data := i.Call.GasLimit, i.Call.FeeCap, i.Call.TipCap, i.Call.Value, i.Call.Data
	if c := p.Cancellation; c != nil {
		to = common.Address(i.Call.Sender)
		gas, fee, tip, value, data = c.GasLimit, c.FeeCap, c.TipCap, new(big.Int), nil
	}
	return types.NewTx(&types.DynamicFeeTx{ChainID: new(big.Int).SetUint64(i.Chain.ChainID), Nonce: p.Nonce, To: &to, Gas: gas, GasFeeCap: fee, GasTipCap: tip, Value: value, Data: data})
}

func variantBudget(p PendingTransaction) *big.Int {
	if p.Cancellation != nil {
		return p.Cancellation.Budget
	}
	if p.Maintenance != nil {
		return p.Maintenance.Budget
	}
	return p.Intent.Budget
}
func pendingBudget(p PendingTransaction) *big.Int {
	budget := new(big.Int).Set(variantBudget(p))
	for _, v := range p.Previous {
		if variantBudget(v).Cmp(budget) > 0 {
			budget.Set(variantBudget(v))
		}
	}
	return budget
}

// updatePending owns d.mu. The reservation is the largest possible cost of
// any mutually exclusive variant, including a cancellation or older route.
func (d *Dispatcher) updatePending(ctx context.Context, next PendingTransaction, check func() error) (domain.Hash, error) {
	state := d.store.state
	old := state.Pending
	prior := *old
	prior.Previous = nil
	next.Previous = append(append([]PendingTransaction{}, old.Previous...), prior)
	if len(next.Previous) > 4 {
		return domain.Hash{}, fmt.Errorf("%w: variant limit", ErrDispatchRejected)
	}
	now := time.Now()
	day := now.Unix() / 86400
	charged := new(big.Int).Set(state.Charged)
	if day > state.Day {
		charged.Set(inFlight(state))
	} else if day < state.Day {
		return domain.Hash{}, fmt.Errorf("dispatch clock moved backwards")
	}
	charged.Add(charged, new(big.Int).Sub(pendingBudget(next), pendingBudget(*old)))
	if variantBudget(next).Cmp(d.limits.PerTransaction) > 0 || charged.Cmp(d.limits.PerDay) > 0 {
		return domain.Hash{}, fmt.Errorf("%w: variant fee budget", ErrDispatchRejected)
	}
	if err := check(); err != nil {
		return domain.Hash{}, err
	}
	unsigned := pendingEnvelope(next)
	signed, err := d.signer.Sign(ctx, unsigned)
	if err != nil {
		return domain.Hash{}, err
	}
	if err = verifySigned(unsigned, signed, d.limits.Sender); err != nil {
		return domain.Hash{}, err
	}
	if err = check(); err != nil {
		return domain.Hash{}, err
	}
	next.Raw, err = signed.MarshalBinary()
	if err != nil {
		return domain.Hash{}, err
	}
	next.Hash = domain.Hash(signed.Hash())
	next.SubmittedMS = now.UnixMilli()
	state.Pending = &next
	state.Charged, state.Day = charged, day
	got, err := d.persistAndSend(ctx, state, next.Hash, next.Raw)
	if errors.Is(err, errJournalUnavailable) {
		return domain.Hash{}, err
	}
	if err != nil {
		return next.Hash, fmt.Errorf("%w; variants retained: %v", ErrSubmissionUnknown, err)
	}
	if got != next.Hash {
		return next.Hash, fmt.Errorf("%w: variant hash mismatch", ErrSubmissionUnknown)
	}
	return next.Hash, nil
}
