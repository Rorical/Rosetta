package exec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Maintenance kinds. Profit stays in the executor as wrapped native, while gas
// is paid in native from the sender: a sweep moves profit to the owner (the
// sender) and an unwrap turns it into native.
const (
	MaintenanceSweep  = "sweep"
	MaintenanceUnwrap = "unwrap"
)

var (
	sweepSelector    = []byte{0x6e, 0xa0, 0x56, 0xa9} // sweep(address,uint256)
	withdrawSelector = []byte{0x2e, 0x1a, 0x7d, 0x4d} // withdraw(uint256)
)

// Maintenance is an owner transaction on the dispatch nonce lane. It shares
// the lane so the journal accounts for every nonce, pays from the daily fee
// budget, and never counts as an arbitrage outcome. Its calldata is one of
// two fixed encodings; validate rejects anything else, so neither a caller nor
// an edited journal can sign an arbitrary call.
type Maintenance struct {
	Kind                   string
	Token                  domain.Address // swept token; the wrapped native for an unwrap
	Amount                 *big.Int
	GasLimit               uint64
	FeeCap, TipCap, Budget *big.Int
	CreatedMS              int64
}

// NewMaintenance prices a sweep or unwrap at feeCap and tipCap.
func NewMaintenance(kind string, token domain.Address, amount *big.Int, gasLimit uint64, feeCap, tipCap *big.Int, now time.Time) Maintenance {
	budget := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), feeCap)
	return Maintenance{Kind: kind, Token: token, Amount: new(big.Int).Set(amount), GasLimit: gasLimit, FeeCap: new(big.Int).Set(feeCap), TipCap: new(big.Int).Set(tipCap), Budget: budget, CreatedMS: now.UnixMilli()}
}

func (m Maintenance) id() domain.Hash {
	var raw bytes.Buffer
	raw.WriteString("maintenance:" + m.Kind)
	raw.Write(m.Token[:])
	raw.Write(m.Amount.Bytes())
	_ = binary.Write(&raw, binary.BigEndian, m.CreatedMS)
	return domain.Hash(sha256.Sum256(raw.Bytes()))
}

// Call returns the target and calldata: the executor's owner sweep, or the
// wrapped native's withdraw.
func (m Maintenance) Call(executor domain.Address) (domain.Address, []byte) {
	word := func(v []byte) []byte { return append(make([]byte, 32-len(v)), v...) }
	if m.Kind == MaintenanceSweep {
		return executor, append(append(append([]byte{}, sweepSelector...), word(m.Token[:])...), word(m.Amount.Bytes())...)
	}
	return m.Token, append(append([]byte{}, withdrawSelector...), word(m.Amount.Bytes())...)
}

func (m Maintenance) validate(limits DispatchLimits) error {
	if m.Amount == nil || m.Amount.Sign() <= 0 || m.Amount.BitLen() > 256 || m.FeeCap == nil || m.TipCap == nil || m.Budget == nil || m.FeeCap.Sign() <= 0 || m.TipCap.Sign() < 0 || m.TipCap.Cmp(m.FeeCap) > 0 || m.GasLimit < 21000 || m.GasLimit > 1_000_000 || m.CreatedMS <= 0 {
		return fmt.Errorf("%w: invalid maintenance", ErrDispatchRejected)
	}
	if m.Budget.Cmp(new(big.Int).Mul(new(big.Int).SetUint64(m.GasLimit), m.FeeCap)) < 0 {
		return fmt.Errorf("%w: maintenance budget omits gas", ErrDispatchRejected)
	}
	switch m.Kind {
	case MaintenanceSweep:
		if _, ok := limits.Assets[m.Token]; !ok {
			return fmt.Errorf("%w: sweep of a non-start asset", ErrDispatchRejected)
		}
	case MaintenanceUnwrap:
		if limits.Wrapped == (domain.Address{}) || m.Token != limits.Wrapped {
			return fmt.Errorf("%w: unwrap of a token other than wrapped native", ErrDispatchRejected)
		}
	default:
		return fmt.Errorf("%w: unknown maintenance %q", ErrDispatchRejected, m.Kind)
	}
	return nil
}

// carrier is the identity the nonce lane records for a maintenance nonce. Its
// conversion is native 1:1, so a settlement's Net is minus the paid fee.
func (m Maintenance) carrier(limits DispatchLimits) ReactiveIntent {
	var i ReactiveIntent
	i.ID, i.Chain, i.Budget, i.CreatedMS = m.id(), limits.Chain, m.Budget, m.CreatedMS
	i.Call.Sender, i.Call.Executor = limits.Sender, limits.Executor
	i.Conversion = FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}
	return i
}

// Maintain signs and sends one maintenance transaction on the next nonce. It
// takes the lane only when it is free, like Dispatch, and passes the same stop
// file, halt and gate checks; current is rechecked after signing.
func (d *Dispatcher) Maintain(ctx context.Context, m Maintenance, current func() bool) (domain.Hash, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.store.state
	if state.Pending != nil {
		if !d.Included() || len(state.Included) >= maxIncluded {
			return domain.Hash{}, ErrDispatchBusy
		}
		state = releaseIncluded(state, time.Now())
	}
	check := func() error {
		if err := d.checkControl(ctx, current); err != nil {
			return err
		}
		if err := m.validate(d.limits); err != nil {
			return err
		}
		if m.Budget.Cmp(d.limits.PerTransaction) > 0 {
			return fmt.Errorf("%w: maintenance above transaction budget", ErrDispatchRejected)
		}
		return nil
	}
	if err := check(); err != nil {
		return domain.Hash{}, err
	}
	if state.NextNonce == ^uint64(0) {
		return domain.Hash{}, fmt.Errorf("nonce exhausted")
	}
	now := time.Now()
	day := now.Unix() / 86400
	charged := new(big.Int).Set(state.Charged)
	if day > state.Day {
		charged.Set(inFlight(state))
	} else if day < state.Day {
		return domain.Hash{}, fmt.Errorf("dispatch clock moved backwards")
	}
	charged.Add(charged, m.Budget)
	if charged.Cmp(d.limits.PerDay) > 0 {
		return domain.Hash{}, fmt.Errorf("%w: daily loss budget", ErrDispatchRejected)
	}
	pending := PendingTransaction{Intent: m.carrier(d.limits), Maintenance: &m, Nonce: state.NextNonce, SubmittedMS: now.UnixMilli()}
	tx := pendingEnvelope(pending)
	signed, err := d.signer.Sign(ctx, tx)
	if err != nil {
		return domain.Hash{}, err
	}
	if err = verifySigned(tx, signed, d.limits.Sender); err != nil {
		return domain.Hash{}, err
	}
	if err = check(); err != nil {
		return domain.Hash{}, err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return domain.Hash{}, err
	}
	hash := domain.Hash(signed.Hash())
	pending.Hash, pending.Raw = hash, raw
	state.Day, state.Charged = day, charged
	d.submissions.Clear()
	state.Pending = &pending
	got, err := d.persistAndSend(ctx, state, hash, raw)
	if errors.Is(err, errJournalUnavailable) {
		return domain.Hash{}, err
	}
	if err != nil {
		return hash, fmt.Errorf("%w; nonce retained: %v", ErrSubmissionUnknown, err)
	}
	if got != hash {
		return hash, fmt.Errorf("%w: response hash mismatch; nonce retained", ErrSubmissionUnknown)
	}
	return hash, nil
}

func maintenanceEnvelope(p PendingTransaction) *types.Transaction {
	m := p.Maintenance
	to, data := m.Call(p.Intent.Call.Executor)
	target := common.Address(to)
	return types.NewTx(&types.DynamicFeeTx{ChainID: new(big.Int).SetUint64(p.Intent.Chain.ChainID), Nonce: p.Nonce, To: &target, Gas: m.GasLimit, GasFeeCap: m.FeeCap, GasTipCap: m.TipCap, Value: new(big.Int), Data: data})
}
