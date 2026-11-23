package exec

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Cancellation is a separately admitted zero-value self transfer. Its cost
// never appears as a successful arbitrage execution or calibration sample.
type Cancellation struct {
	EstimatedFailureFee    *big.Int
	Target                 domain.Hash
	FeeCap, TipCap, Budget *big.Int
	GasLimit               uint64
	CreatedMS, ExpiresMS   int64
	Reason                 string
}

func (c Cancellation) validate(now time.Time) error {
	if c.Target == (domain.Hash{}) || c.Reason == "" || c.GasLimit < 21000 || c.GasLimit > 32000000 || c.FeeCap == nil || c.TipCap == nil || c.Budget == nil || c.FeeCap.Sign() <= 0 || c.TipCap.Sign() < 0 || c.TipCap.Cmp(c.FeeCap) > 0 || c.CreatedMS <= 0 || now.UnixMilli() < c.CreatedMS || now.UnixMilli() >= c.ExpiresMS || c.ExpiresMS-c.CreatedMS > 2000 {
		return fmt.Errorf("%w: invalid cancellation", ErrDispatchRejected)
	}
	if c.EstimatedFailureFee == nil || c.Budget.Cmp(c.EstimatedFailureFee) >= 0 {
		return fmt.Errorf("%w: cancellation lacks cost advantage", ErrDispatchRejected)
	}
	maximum := new(big.Int).Mul(new(big.Int).SetUint64(c.GasLimit), c.FeeCap)
	if c.Budget.Cmp(maximum) < 0 {
		return fmt.Errorf("%w: cancellation budget omits gas", ErrDispatchRejected)
	}
	return nil
}

func (d *Dispatcher) Cancel(ctx context.Context, c Cancellation, current func() bool) (domain.Hash, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	old := d.store.state.Pending
	if old == nil || old.Cancellation != nil || old.Maintenance != nil || old.Hash != c.Target {
		return domain.Hash{}, fmt.Errorf("%w: cancellation target changed", ErrDispatchRejected)
	}
	check := func() error {
		if d.Included() {
			return fmt.Errorf("%w: transaction already observed", ErrDispatchRejected)
		}
		if err := d.checkControl(ctx, current); err != nil {
			return err
		}
		return c.validate(time.Now())
	}
	if err := check(); err != nil {
		return domain.Hash{}, err
	}
	if c.FeeCap.Cmp(ReplacementFee(old.Intent.Call.FeeCap)) < 0 || c.TipCap.Cmp(ReplacementFee(old.Intent.Call.TipCap)) < 0 {
		return domain.Hash{}, fmt.Errorf("%w: cancellation fee bump", ErrDispatchRejected)
	}
	return d.updatePending(ctx, PendingTransaction{Intent: old.Intent, Nonce: old.Nonce, Cancellation: &c}, check)
}
