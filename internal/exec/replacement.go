package exec

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Replace substitutes a freshly admitted, economically better action at the
// unresolved nonce. Every signed variant remains eligible for reconciliation.
func (d *Dispatcher) Replace(ctx context.Context, i ReactiveIntent, current func() bool) (domain.Hash, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	old := d.store.state.Pending
	if old == nil || old.Cancellation != nil || old.Maintenance != nil || d.Included() {
		return domain.Hash{}, fmt.Errorf("%w: nonce cannot be replaced", ErrDispatchRejected)
	}
	// A conditional send returns once it has executed or been dropped, so a
	// pending one has an unknown outcome; a replacement could land beside it.
	if old.Intent.Conditions != nil || i.Conditions != nil {
		return domain.Hash{}, fmt.Errorf("%w: conditional transactions are not replaced", ErrDispatchRejected)
	}
	if err := d.check(ctx, i, current); err != nil {
		return domain.Hash{}, err
	}
	if len(old.Previous) >= 3 || !BetterReactive(i, old.Intent, d.limits.ReplacementGainBPS) {
		return domain.Hash{}, fmt.Errorf("%w: replacement limit or insufficient net gain", ErrDispatchRejected)
	}
	if i.Call.FeeCap.Cmp(ReplacementFee(old.Intent.Call.FeeCap)) < 0 || i.Call.TipCap.Cmp(ReplacementFee(old.Intent.Call.TipCap)) < 0 {
		return domain.Hash{}, fmt.Errorf("%w: replacement fee bump below 10 percent", ErrDispatchRejected)
	}
	next := PendingTransaction{Intent: i, Nonce: old.Nonce}
	check := func() error {
		if d.Included() {
			return fmt.Errorf("%w: transaction already observed", ErrDispatchRejected)
		}
		return d.check(ctx, i, current)
	}
	return d.updatePending(ctx, next, check)
}

// ReplacementFee is the local minimum. Endpoint acceptance is verified by
// acknowledgement and inclusion; a particular sequencer can require more.
func ReplacementFee(prior *big.Int) *big.Int {
	minimum := new(big.Int).Mul(prior, big.NewInt(110))
	minimum.Add(minimum, big.NewInt(99)).Quo(minimum, big.NewInt(100))
	if minimum.Cmp(prior) <= 0 {
		minimum.Add(prior, big.NewInt(1))
	}
	return minimum
}

// BetterReactive compares conservative native-valued net after the new bid.
// Cross multiplication avoids rounding away small differences across assets.
func BetterReactive(next, prior ReactiveIntent, gainBPS uint64) bool {
	a := new(big.Int).Mul(next.ExpectedNet, next.Conversion.Denominator)
	a.Mul(a, prior.Conversion.Numerator).Mul(a, big.NewInt(10000))
	b := new(big.Int).Mul(prior.ExpectedNet, prior.Conversion.Denominator)
	b.Mul(b, next.Conversion.Numerator)
	strict := new(big.Int).Mul(b, big.NewInt(10000))
	b.Mul(b, new(big.Int).SetUint64(10000+gainBPS))
	return a.Cmp(strict) > 0 && a.Cmp(b) >= 0
}
