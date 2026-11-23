package exec

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var ErrSubmissionUnknown = errors.New("submission outcome unknown")

var ErrDispatchBusy = errors.New("nonce lane has an unresolved transaction")
var ErrDispatchRejected = errors.New("reactive dispatch rejected")

// ErrConditionsRejected is a conditional send the sequencer dropped (-32003):
// it used neither gas nor its nonce.
var ErrConditionsRejected = errors.New("transaction conditions rejected")

// DispatchNetwork separates shared nonce/budget ownership from chain RPC.
// Settlement reads it only off the send path, once per settled transaction
// or when a transaction goes missing.
type DispatchNetwork interface {
	PendingNonce(context.Context, domain.Address) (uint64, error)
	ConfirmedNonce(context.Context, domain.Address) (uint64, error)
	// Send uses eth_sendRawTransactionConditional when conditions are set and
	// reports their rejection as ErrConditionsRejected.
	Send(context.Context, []byte, *TransactionConditions) (domain.Hash, error)
	// Receipt is the transaction's receipt on the node's current chain, or nil.
	Receipt(context.Context, domain.Hash) (*rpc.TransactionReceipt, error)
	// Diagnose explains a reverted executor call and may record a token
	// exclusion; an empty result means no diagnosis.
	Diagnose(context.Context, ReactiveIntent, *Settlement) string
}

type DispatchLimits struct {
	TokenCheck             func(domain.Address) error
	PoolCheck              func(domain.PoolKey) error // quarantined pools
	ReplacementGainBPS     uint64
	Chain                  domain.ChainIdentity
	Sender, Executor       domain.Address
	CodeHash               domain.Hash
	PerTransaction, PerDay *big.Int
	Assets                 map[domain.Address]*big.Int
	StopFile               string
	// Wrapped is the wrapped native a maintenance unwrap may withdraw from;
	// zero allows no unwrap.
	Wrapped domain.Address
}

type Dispatcher struct {
	pending       atomic.Pointer[PendingTransaction]
	watch         atomic.Pointer[watchSet] // every unfinished nonce's variants, for lock-free feed matching
	submissions   sync.Map                 // hash -> immutable SubmissionTiming; at most one nonce lane
	observedNonce atomic.Uint64
	mu            sync.Mutex
	store         *dispatchStore
	signer        TransactionSigner
	network       DispatchNetwork
	limits        DispatchLimits
	canonical     canonicalView
	// Follow-up RPC runs in tasks under ctx; their results return through
	// results and are applied by the owner's goroutine. running is guarded by mu.
	ctx     context.Context
	cancel  context.CancelFunc
	tasks   sync.WaitGroup
	results chan Followup
	running map[taskKey]bool
	timing  settleTiming
}

func OpenDispatcher(ctx context.Context, path string, limits DispatchLimits, signer TransactionSigner, network DispatchNetwork) (*Dispatcher, error) {
	if limits.ReplacementGainBPS > 10000 || signer == nil || network == nil || limits.Sender != signer.Address() || limits.Executor == (domain.Address{}) || limits.CodeHash == (domain.Hash{}) || limits.PerTransaction == nil || limits.PerTransaction.Sign() <= 0 || limits.PerDay == nil || limits.PerDay.Cmp(limits.PerTransaction) < 0 || len(limits.Assets) == 0 || limits.StopFile == "" || limits.TokenCheck == nil {
		return nil, fmt.Errorf("incomplete dispatch limits")
	}
	n, err := network.PendingNonce(ctx, limits.Sender)
	if err != nil {
		return nil, err
	}
	latest, err := network.ConfirmedNonce(ctx, limits.Sender)
	if err != nil {
		return nil, err
	}
	s, err := openDispatchStore(path, DispatchState{Chain: limits.Chain, Sender: limits.Sender, Executor: limits.Executor, NextNonce: n, Charged: new(big.Int)}, limits)
	if err != nil {
		return nil, err
	}
	// Released transactions may still be absent from the latest block, so the
	// confirmed nonce can trail the journal by at most their count.
	released := uint64(len(s.state.Included))
	if s.state.Pending == nil && (s.state.NextNonce != n || latest > n || latest+released < n) {
		s.close()
		return nil, fmt.Errorf("operator nonce changed outside journal; reconcile before resuming")
	}
	if s.state.Pending != nil && (latest+released < s.state.Pending.Nonce || latest > s.state.Pending.Nonce+1) {
		s.close()
		return nil, fmt.Errorf("untracked nonce activity while transaction unresolved")
	}
	d := &Dispatcher{store: s, signer: signer, network: network, limits: limits, results: make(chan Followup, 16), running: map[taskKey]bool{}, timing: defaultSettleTiming}
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.publish(s.state)
	return d, nil
}

// Close stops follow-up tasks; their unapplied results are dropped and the
// journal keeps what they would have resolved.
func (d *Dispatcher) Close() error {
	d.cancel()
	d.tasks.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.store.close()
}
func (d *Dispatcher) Busy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.store.state.Pending != nil || d.store.state.HaltedReason != ""
}

// Dispatch writes the signed bytes to the journal while sending them. An error
// from Send or from that write is ambiguous and keeps the nonce and full cost
// reserved. No blind automatic replacement or reuse is permitted.
func (d *Dispatcher) Dispatch(ctx context.Context, i ReactiveIntent, current func() bool) (domain.Hash, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.store.state
	if state.Pending != nil {
		// A variant seen in an accepted prefix has consumed its nonce. The lane
		// opens for the next nonce before the sighting is booked; the released
		// one stays watched until a canonical block confirms it.
		if !d.Included() || len(state.Included) >= maxIncluded {
			return domain.Hash{}, ErrDispatchBusy
		}
		state = releaseIncluded(state, time.Now())
	}
	if err := d.check(ctx, i, current); err != nil {
		return domain.Hash{}, err
	}
	if state.NextNonce == ^uint64(0) {
		return domain.Hash{}, fmt.Errorf("nonce exhausted")
	}
	for _, id := range state.Completed {
		if id == i.ID {
			return domain.Hash{}, fmt.Errorf("%w: duplicate intent", ErrDispatchRejected)
		}
	}
	if sentIntent(state, i.ID) {
		return domain.Hash{}, fmt.Errorf("%w: duplicate intent", ErrDispatchRejected)
	}
	now := time.Now()
	day := now.Unix() / 86400
	charged := new(big.Int).Set(state.Charged)
	if day > state.Day {
		charged.Set(inFlight(state))
	} else if day < state.Day {
		return domain.Hash{}, fmt.Errorf("dispatch clock moved backwards")
	}
	charged.Add(charged, i.Budget)
	if charged.Cmp(d.limits.PerDay) > 0 {
		return domain.Hash{}, fmt.Errorf("%w: daily loss budget", ErrDispatchRejected)
	}
	to := common.Address(i.Call.Executor)
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: new(big.Int).SetUint64(i.Chain.ChainID), Nonce: state.NextNonce, GasTipCap: i.Call.TipCap, GasFeeCap: i.Call.FeeCap, Gas: i.Call.GasLimit, To: &to, Value: i.Call.Value, Data: i.Call.Data})
	signed, err := d.signer.Sign(ctx, tx)
	if err != nil {
		return domain.Hash{}, err
	}
	if err = verifySigned(tx, signed, d.limits.Sender); err != nil {
		return domain.Hash{}, err
	}
	if err = d.check(ctx, i, current); err != nil {
		return domain.Hash{}, err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return domain.Hash{}, err
	}
	hash := domain.Hash(signed.Hash())
	unsent := state
	state.Day, state.Charged = day, charged
	d.submissions.Clear()
	state.Pending = &PendingTransaction{Intent: i, Hash: hash, Raw: raw, Nonce: state.NextNonce, SubmittedMS: now.UnixMilli()}
	got, err := d.persistAndSend(ctx, state, hash, raw)
	if errors.Is(err, errJournalUnavailable) {
		return domain.Hash{}, err
	}
	// A journal write that failed during the send leaves the outcome unknown
	// below, even for a rejection.
	if errors.Is(err, ErrConditionsRejected) && !errors.Is(err, ErrSubmissionUnknown) {
		// The nonce and budget were never used: restore the lane as it was.
		d.store.state = unsent
		d.publish(unsent)
		if saveErr := d.store.save(unsent); saveErr != nil {
			return domain.Hash{}, fmt.Errorf("%w: journal write failed after a rejected send: %v", errJournalUnavailable, saveErr)
		}
		return domain.Hash{}, fmt.Errorf("%w: %s %w", ErrDispatchRejected, hash.Hex(), err)
	}
	if err != nil {
		return hash, fmt.Errorf("%w; nonce retained: %v", ErrSubmissionUnknown, err)
	}
	if got != hash {
		return hash, fmt.Errorf("%w: response hash mismatch; nonce retained", ErrSubmissionUnknown)
	}
	return hash, nil
}

var errJournalUnavailable = errors.New("dispatch journal requires restart")

// persistAndSend broadcasts while the journal write is in flight, instead of
// after it: the write and fsync typically cost 5–10 ms. A crash inside that
// window leaves a sent nonce unrecorded;
// OpenDispatcher then refuses to resume until the nonce is reconciled, so it is
// never reused blindly. Memory takes the new pending state before the send,
// and a failed journal refuses every later send until restart.
func (d *Dispatcher) persistAndSend(ctx context.Context, state DispatchState, hash domain.Hash, raw []byte) (domain.Hash, error) {
	if d.store.failed {
		return domain.Hash{}, errJournalUnavailable
	}
	// Set before the writer starts; this goroutine reads store state again only
	// after the write returns.
	d.store.state = state
	d.publish(state)
	saved := make(chan error, 1)
	go func(state DispatchState) { saved <- d.store.save(state) }(state)
	var conditions *TransactionConditions
	if state.Pending != nil && state.Pending.Hash == hash {
		conditions = state.Pending.Intent.Conditions
	}
	got, err := d.send(ctx, hash, raw, conditions)
	if saveErr := <-saved; saveErr != nil {
		return got, fmt.Errorf("journal write failed while sending: %v; %w", saveErr, errors.Join(err, ErrSubmissionUnknown))
	}
	return got, err
}

func (d *Dispatcher) check(ctx context.Context, i ReactiveIntent, current func() bool) error {
	if err := d.checkControl(ctx, current); err != nil {
		return err
	}
	if err := CheckRouteTokens(i.Route, d.limits.TokenCheck); err != nil {
		return fmt.Errorf("%w: %v", ErrDispatchRejected, err)
	}
	if err := validateReactivePoolFees(i); err != nil {
		return fmt.Errorf("%w: %v", ErrDispatchRejected, err)
	}
	if err := i.Validate(ctx, time.Now()); err != nil {
		return fmt.Errorf("%w: %v", ErrDispatchRejected, err)
	}
	if i.Chain != d.limits.Chain || i.Call.Sender != d.limits.Sender || i.Call.Executor != d.limits.Executor || i.Call.CodeHash != d.limits.CodeHash || i.Budget.Cmp(d.limits.PerTransaction) > 0 {
		return fmt.Errorf("%w: identity or transaction budget", ErrDispatchRejected)
	}
	limit := d.limits.Assets[i.Route.Asset]
	if limit == nil || i.Route.Amount.Cmp(limit) > 0 {
		return fmt.Errorf("%w: principal limit", ErrDispatchRejected)
	}
	// Only the start asset is allowlisted with a principal cap. Pools and
	// intermediate tokens come from discovery; the executor checks balances and
	// profit on chain, and CheckRouteTokens above excludes shortfall tokens.
	for _, step := range i.Route.Steps {
		if d.limits.PoolCheck != nil {
			if err := d.limits.PoolCheck(step.Pool); err != nil {
				return fmt.Errorf("%w: %v", ErrDispatchRejected, err)
			}
		}
	}
	if i.Route.Funding == FundingV3Flash && d.limits.PoolCheck != nil {
		if err := d.limits.PoolCheck(i.Route.Lender); err != nil {
			return fmt.Errorf("%w: lender: %v", ErrDispatchRejected, err)
		}
	}
	return nil
}

func (d *Dispatcher) checkControl(ctx context.Context, current func() bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.store.state.HaltedReason != "" {
		return fmt.Errorf("%w: %s", ErrDispatchRejected, d.store.state.HaltedReason)
	}
	if stopped, err := d.stopRequested(); err != nil {
		return err
	} else if stopped {
		return fmt.Errorf("%w: stop file present", ErrDispatchRejected)
	}
	if current == nil || !current() {
		return fmt.Errorf("%w: pool state changed", ErrDispatchRejected)
	}
	return nil
}

func (d *Dispatcher) stopRequested() (bool, error) {
	if _, err := os.Stat(d.limits.StopFile); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	return false, nil
}

func appendBounded(ids []domain.Hash, id domain.Hash) []domain.Hash {
	ids = append(ids, id)
	if len(ids) > 256 {
		ids = ids[len(ids)-256:]
	}
	return ids
}

// LastSettlement permits idempotent sample recovery after a crash between the
// nonce journal commit and the sample journal append.
func (d *Dispatcher) LastSettlement() *Settlement {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.store.state.LastReceipt
}
