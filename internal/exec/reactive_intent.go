package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
	"math/big"
	"time"
)

// ReactiveIntent is explicitly unconfirmed. It carries a conservative cost
// budget and contract guards, never fabricated EVM evidence or ready status.
type ReactiveIntent struct {
	PoolFees         []ReactivePoolFee `json:",omitempty"`
	Dependencies     []domain.PoolKey  `json:",omitempty"`
	QuotedGross      *big.Int
	QuoteReserve     *big.Int
	ID               domain.Hash
	Chain            domain.ChainIdentity
	Version          domain.StateVersion
	Route            ExecutorPlan
	Call             ExecutorCall
	Budget           *big.Int
	ExpectedNet      *big.Int
	MinimumNet       *big.Int
	Conversion       FeeConversion
	CreatedMS        int64
	ExpiresMS        int64
	ContextExpiresMS int64
	Bid              *BidDecision `json:",omitempty"`
	// TipGas is the expected gas the profit guard charges the fee cap on;
	// zero means the whole limit. Budget still covers the limit at the full
	// fee cap, so the spend limits keep the worst case. A transaction that
	// uses more than TipGas can land below MinimumNet by the fee cap on the
	// excess.
	TipGas uint64 `json:",omitempty"`
	// Conditions send the transaction through eth_sendRawTransactionConditional;
	// a hot route requires them.
	Conditions *TransactionConditions `json:",omitempty"`
}

// BidDecision records the competition model's priority fee for this intent:
// the tip that maximized expected profit against the modelled rivals, its win
// probability, and whether the signed tip used it.
//
// Wins, Samples, Route, Similar, Rival, RivalN and RivalBid belong to the
// earlier window-count model. They stay so that intents persisted
// by it re-encode to the same identity; new decisions leave them zero.
type BidDecision struct {
	Learning *BidLearning `json:",omitempty"`

	CostVersion string `json:",omitempty"`
	DecisionNS  int64  `json:",omitempty"`
	AgeNS       int64  `json:",omitempty"`
	Lane        string `json:",omitempty"`
	RevertGas   uint64 `json:",omitempty"`
	// ContestFloor is the lowest tip considered because recent contests on
	// the route's pools paid that share of an opportunity's worth; 0 without.
	ContestFloor    uint64 `json:",omitempty"`
	CostEvaluations int    `json:",omitempty"`
	CostTruncated   bool   `json:",omitempty"`
	ModelTip        *big.Int
	Wins, Samples   int
	Expected        *big.Int // expected native profit per attempt at ModelTip
	Applied         bool
	Route           bool   `json:",omitempty"`
	Gas             uint64 `json:",omitempty"` // expected gas used that priced the tip
	// Cap is the highest tip the surplus, fee cap and budget allowed; Worth is
	// the surplus per expected gas.
	Cap      *big.Int `json:",omitempty"`
	Worth    uint64   `json:",omitempty"`
	Similar  bool     `json:",omitempty"`
	Rival    string   `json:",omitempty"`
	RivalN   int      `json:",omitempty"`
	RivalBid uint64   `json:",omitempty"`
	// Win is the calibrated probability that ModelTip wins, WinRaw the model's
	// before calibration and Shift the logit calibration between them:
	// Worth, local decision age and lane contribute to Shift; the legacy
	// ShiftIntercept/ShiftSlope/ShiftCenter fields describe its worth component, clamped to
	// the range of the calibrating outcomes. First is the chance a rival lands
	// before us, Unknown the presence of a rival not seen before.
	Win            float64    `json:",omitempty"`
	WinRaw         float64    `json:",omitempty"`
	Shift          float64    `json:",omitempty"`
	ShiftIntercept float64    `json:",omitempty"`
	ShiftSlope     float64    `json:",omitempty"`
	ShiftCenter    uint64     `json:",omitempty"` // wei per gas
	First          float64    `json:",omitempty"`
	Unknown        float64    `json:",omitempty"`
	Rivals         []BidRival `json:",omitempty"` // the three that cost the most win probability
	// Reason is why the model declines to send; Probe marks a zero tip sent
	// against a predatory rival to see whether it is still there.
	Reason string `json:",omitempty"`
	Probe  bool   `json:",omitempty"`
	// Window is the start (Unix ms) of the admission window this decision
	// priced; our outcome is attributed to it. Windows and RouteWindows count
	// the closed windows sampled and those sharing a pool with the route.
	Window       int64 `json:",omitempty"`
	Windows      int   `json:",omitempty"`
	RouteWindows int   `json:",omitempty"`
}

// BidRival is one modelled rival: its presence on the route, its median
// predicted bid per gas, the weight of its own history against the global
// share prior, and how often it bids above the opportunity's value.
type BidRival struct {
	Address    string
	Presence   float64
	Bid        uint64
	Own        float64 `json:",omitempty"`
	AboveValue float64 `json:",omitempty"`
}

// SealReactiveIntent fixes the exact unsigned route. The dispatcher fills its
// locally owned nonce and verifies the signing result against this envelope.
func SealReactiveIntent(ctx context.Context, i ReactiveIntent) (ReactiveIntent, error) {
	if err := validateReactivePoolFees(i); err != nil {
		return ReactiveIntent{}, err
	}
	i.ID = domain.Hash{}
	if err := i.validate(ctx, time.UnixMilli(i.CreatedMS)); err != nil {
		return ReactiveIntent{}, err
	}
	encoded, err := json.Marshal(i)
	if err != nil {
		return ReactiveIntent{}, err
	}
	i.ID = keccak(encoded)
	return i, nil
}
func (i ReactiveIntent) validate(ctx context.Context, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.Chain.ChainID == 0 || (i.Chain.Family != domain.FamilyOPStack && i.Chain.Family != domain.FamilyNitro) || i.CreatedMS <= 0 || now.UnixMilli() < i.CreatedMS || now.UnixMilli() >= i.ExpiresMS || i.ExpiresMS > i.ContextExpiresMS || i.ExpiresMS-i.CreatedMS > 2000 {
		return fmt.Errorf("invalid or stale reactive context")
	}
	if i.Version.Anchor.Hash == (domain.Hash{}) || i.Version.PrefixHash == (domain.Hash{}) {
		return fmt.Errorf("missing reactive state identity")
	}
	if i.Budget == nil || i.Budget.Sign() <= 0 || i.MinimumNet == nil || i.MinimumNet.Sign() <= 0 || i.ExpectedNet == nil || i.ExpectedNet.Cmp(i.MinimumNet) < 0 {
		return fmt.Errorf("reactive economics unavailable or non-positive")
	}
	call, err := BuildExecutorCall(ctx, i.Route, i.Call.CallParameters)
	if err != nil {
		return err
	}
	if i.QuotedGross == nil || i.QuoteReserve == nil || i.QuoteReserve.Sign() < 0 {
		return fmt.Errorf("missing reactive quote economics")
	}
	minimum := new(big.Int).Mul(new(big.Int).SetUint64(i.Call.GasLimit), i.Call.FeeCap)
	if i.Budget.Cmp(minimum) < 0 {
		return fmt.Errorf("reactive budget omits maximum execution gas cost")
	}
	if i.TipGas > i.Call.GasLimit {
		return fmt.Errorf("reactive tip gas exceeds the gas limit")
	}
	cost, err := FeeInAsset(i.GuardFee(), i.Conversion)
	if err != nil {
		return err
	}
	net, err := CheckQuotedProfit(i.QuotedGross, i.QuoteReserve, i.Route.MaxLoanFee, cost, i.MinimumNet)
	if err != nil {
		return err
	}
	if net.Cmp(i.ExpectedNet) != 0 {
		return fmt.Errorf("reactive net differs from quote, funding and fee budget")
	}
	if i.Route.MinProfit.Cmp(new(big.Int).Add(cost, i.MinimumNet)) < 0 {
		return fmt.Errorf("contract profit guard omits fee budget")
	}
	if i.Route.Deadline > uint64(^uint64(0)>>1)/1000 || i.Route.Deadline*1000 < uint64(i.ExpiresMS) {
		return fmt.Errorf("contract expires before reactive intent")
	}
	// executeV3Hot checks neither pause nor deadline: without these conditions a
	// paused executor would still run it.
	if i.Route.Hot && (i.Conditions == nil || i.Conditions.TimestampMax == 0 || i.Conditions.TimestampMax > i.Route.Deadline || !i.Conditions.holdsSlot(i.Call.Executor, domain.Hash{})) {
		return fmt.Errorf("a hot route needs the executor's slot 0 and its deadline as conditions")
	}
	if i.Route.Claim && !i.Conditions.holdsSlot(i.Call.Executor, ClaimEpochSlot) {
		return fmt.Errorf("a claiming route needs the executor's claim epoch as a condition")
	}
	if i.Conditions != nil && (i.Conditions.TimestampMax == 0 || i.Conditions.TimestampMax > i.Route.Deadline) {
		return fmt.Errorf("conditions must expire by the route's deadline")
	}
	// A leg neither bounded nor held would let a rival's earlier move run
	// every swap before the profit check reverts.
	for n, step := range i.Route.Steps {
		if i.Route.Hot && step.PriceBound == nil && !i.Conditions.holds(step.Pool.Address) {
			return fmt.Errorf("hot route step %d has neither a price bound nor a condition", n)
		}
	}
	a, _ := json.Marshal(call)
	b, _ := json.Marshal(i.Call)
	if string(a) != string(b) {
		return fmt.Errorf("reactive calldata differs from route")
	}
	return nil
}

// ForSender reseals i for another operator of the same executor, the sender of
// another nonce lane. A wallet-funded route pulls its principal from
// msg.sender, so it keeps its own sender.
func ForSender(ctx context.Context, i ReactiveIntent, sender domain.Address) (ReactiveIntent, error) {
	if i.Call.Sender == sender {
		return i, nil
	}
	if i.Route.Funding == FundingWallet {
		return ReactiveIntent{}, fmt.Errorf("a wallet-funded route keeps its sender")
	}
	i.ID = domain.Hash{}
	i.Call.Sender = sender
	return SealReactiveIntent(ctx, i)
}

// GuardFee is the native fee the profit guard covers: Budget less the fee cap
// on the gas above TipGas.
func (i ReactiveIntent) GuardFee() *big.Int {
	if i.TipGas == 0 {
		return new(big.Int).Set(i.Budget)
	}
	unreserved := new(big.Int).Mul(i.Call.FeeCap, new(big.Int).SetUint64(i.Call.GasLimit-i.TipGas))
	return unreserved.Sub(i.Budget, unreserved)
}

func (i ReactiveIntent) Validate(ctx context.Context, now time.Time) error {
	if err := i.validate(ctx, now); err != nil {
		return err
	}
	copy := i
	copy.ID = domain.Hash{}
	b, err := json.Marshal(copy)
	if err != nil || i.ID == (domain.Hash{}) || keccak(b) != i.ID {
		return fmt.Errorf("reactive intent changed after admission")
	}
	return nil
}
