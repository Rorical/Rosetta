package exec

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Funding selects a funding branch of ArbitrageExecutor.execute.
type Funding uint8

const (
	FundingWallet Funding = iota
	FundingV3Flash
	FundingV3FlashSwap
	FundingV4Flash
	// FundingInventory spends the executor's own balance of the asset.
	FundingInventory
)

// ExecutorStep spends the actual output of the preceding leg. The executor has
// no pool registry: kind, target, tokens, fee and tick spacing travel in
// calldata and the contract's balance and profit checks bound a wrong target.
type ExecutorStep struct {
	Pool              domain.PoolKey
	TokenIn           domain.Address // implied on chain by the previous leg
	TokenOut          domain.Address
	Fee               uint32 // V2 input fee in pips and the V4 key fee
	TickSpacing       int32  // V4 key only
	MinAmountOut      *big.Int
	SqrtPriceLimitX96 *big.Int // nil means the protocol default limit
	// PriceBound is the step's one-sided pre-swap bound (packed version 3,
	// contracts/src/PriceCheck.sol): a sqrt price for V3-family and V4 pools,
	// reserveOut/reserveIn in Q96 for V2 and Velodrome. Nil skips the check.
	// With the plan's MinRate set (version 4) it is the step's price at the
	// quote in the same units instead.
	PriceBound *big.Int `json:",omitempty"`
	// AmountIn and AmountOut are the step's quoted amounts. A chained hot
	// route (EncodeHotChainPlan) sends them: each pool pays the next, so the
	// amounts are fixed before the swaps run.
	AmountIn  *big.Int `json:",omitempty"`
	AmountOut *big.Int `json:",omitempty"`
}

// ExecutorPlan is the contract argument, not evidence of successful execution.
// Sender, executor code hash, state identity and fee assumptions belong to the
// surrounding confirmation request and must also match before dispatch.
type ExecutorPlan struct {
	Funding      Funding
	Lender       domain.PoolKey // only standalone V3 flash funding uses a lender
	LenderToken0 bool           // the lender sends Asset as its token0
	Asset        domain.Address
	Amount       *big.Int
	MaxLoanFee   *big.Int
	Steps        []ExecutorStep
	MinProfit    *big.Int // after loan fees; gas/L1 fees are checked separately
	Deadline     uint64
	// MinRate, when set, selects the cumulative precheck (packed version 4):
	// the least product, in 1e18, of the checked legs' rate changes since the
	// quote, whose prices the steps' PriceBound then hold.
	MinRate *big.Int `json:",omitempty"`
	// Hot selects executeV3Hot (EncodeHotV3Plan). It runs only as a
	// conditional transaction holding HotConditions.
	Hot bool `json:",omitempty"`
	// Chain selects executeHot's chained route for a hot plan
	// (EncodeHotChainPlan) instead of executeV3Hot's.
	Chain bool `json:",omitempty"`
	// Claim, on a hot route, raises the executor's claimEpoch on success; the
	// route's conditions hold that epoch, so the first of an opportunity's
	// variants to succeed drops the rest.
	Claim bool `json:",omitempty"`
}

const executeSignature = "execute((uint8,address,bool,address,uint256,uint256,(uint8,address,address,uint24,int24,uint256,uint160)[],uint256,uint256))"

// CheckExecutorPool reports whether the executor can route through a pool.
// Hooked V4 pools stay unsupported: a hook can change the fee or the swap.
func CheckExecutorPool(key domain.PoolKey) error {
	if key.Address == (domain.Address{}) || key.Kind < domain.PoolKindV2 || key.Kind > domain.PoolKindWrap {
		return fmt.Errorf("invalid executor pool kind or target")
	}
	emptyID := key.PoolID == (domain.Hash{})
	if key.Hooks != (domain.Address{}) || (key.IsV4() && emptyID) || (!key.IsV4() && !emptyID) {
		return fmt.Errorf("invalid executor pool ID or unsupported hooks")
	}
	return nil
}

// EncodeExecutorPlan builds exactly one execute tuple. It performs no signing,
// remote calls or state overrides. Passing this boundary does not mark a plan ready.
func EncodeExecutorPlan(ctx context.Context, plan ExecutorPlan) ([]byte, error) {
	if err := checkExecutorPlan(ctx, plan); err != nil {
		return nil, err
	}
	for _, step := range plan.Steps {
		if step.PriceBound != nil || plan.MinRate != nil {
			return nil, fmt.Errorf("price bounds need the packed executor entry")
		}
	}
	// The outer dynamic tuple starts at byte 32. The steps array contains static
	// seven-word tuples and starts immediately after the nine-word plan head.
	selector := keccak([]byte(executeSignature))
	encoded := make([]byte, 4+32+9*32+32+len(plan.Steps)*7*32)
	copy(encoded, selector[:4])
	putUint64(encoded[4:36], 32)
	head := encoded[36:324]
	head[31] = byte(plan.Funding)
	if plan.Funding == FundingV3Flash {
		copy(head[44:64], plan.Lender.Address[:])
		if plan.LenderToken0 {
			head[95] = 1
		}
	}
	copy(head[108:128], plan.Asset[:])
	plan.Amount.FillBytes(head[128:160])
	plan.MaxLoanFee.FillBytes(head[160:192])
	putUint64(head[192:224], 9*32)
	plan.MinProfit.FillBytes(head[224:256])
	putUint64(head[256:288], plan.Deadline)
	putUint64(encoded[324:356], uint64(len(plan.Steps)))
	for i, step := range plan.Steps {
		word := encoded[356+i*224 : 356+(i+1)*224]
		word[31] = byte(step.Pool.Kind)
		copy(word[44:64], step.Pool.Address[:])
		copy(word[76:96], step.TokenOut[:])
		putUint64(word[96:128], uint64(step.Fee))
		putInt24(word[128:160], step.TickSpacing)
		step.MinAmountOut.FillBytes(word[160:192])
		if step.SqrtPriceLimitX96 != nil {
			step.SqrtPriceLimitX96.FillBytes(word[192:224])
		}
	}
	return encoded, nil
}

// checkExecutorPlan mirrors the contract's route rules so a plan fails here, not
// after paying for an on-chain revert.
func checkExecutorPlan(ctx context.Context, plan ExecutorPlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.Funding > FundingInventory || len(plan.Steps) == 0 || len(plan.Steps) > 16 || plan.Deadline == 0 {
		return fmt.Errorf("invalid executor funding, route length or deadline")
	}
	if err := validUint("amount", plan.Amount, 256, true); err != nil {
		return err
	}
	if err := validUint("min profit", plan.MinProfit, 256, true); err != nil {
		return err
	}
	if err := validUint("max loan fee", plan.MaxLoanFee, 256, false); err != nil {
		return err
	}
	if plan.Funding == FundingV4Flash && len(plan.Steps) < 2 {
		return fmt.Errorf("V4 flash accounting requires a closed route with at least two steps")
	}
	if plan.Funding != FundingV3Flash {
		if plan.Lender != (domain.PoolKey{}) || plan.LenderToken0 || plan.MaxLoanFee.Sign() != 0 {
			return fmt.Errorf("funding without a standalone loan requires zero lender and loan fee")
		}
		if plan.Funding == FundingInventory && plan.Asset == (domain.Address{}) {
			return fmt.Errorf("inventory funding requires an ERC20 asset")
		}
		if plan.Funding == FundingV3FlashSwap && (plan.Asset == (domain.Address{}) || len(plan.Steps) < 2 || (plan.Steps[0].Pool.Kind != domain.PoolKindV3 && plan.Steps[0].Pool.Kind != domain.PoolKindSlipstream && plan.Steps[0].Pool.Kind != domain.PoolKindSolidlyV3)) {
			return fmt.Errorf("flash swap funding requires an ERC20 asset and a V3, Slipstream or Solidly V3 first step in a closed route")
		}
	} else {
		if plan.Lender.Kind != domain.PoolKindV3 || plan.Asset == (domain.Address{}) {
			return fmt.Errorf("flash funding requires a V3 lender and ERC20 asset")
		}
		if err := CheckExecutorPool(plan.Lender); err != nil {
			return fmt.Errorf("lender: %w", err)
		}
	}
	token := plan.Asset
	for i, step := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := CheckExecutorPool(step.Pool); err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		if step.TokenIn != token || step.TokenOut == step.TokenIn {
			return fmt.Errorf("step %d: route tokens are disconnected", i)
		}
		if plan.Funding == FundingV3Flash && step.Pool.Address == plan.Lender.Address {
			return fmt.Errorf("step %d uses the locked flash lender", i)
		}
		if plan.Funding == FundingV3FlashSwap && i > 0 && step.Pool.Address == plan.Steps[0].Pool.Address {
			return fmt.Errorf("step %d reuses the locked flash swap pool", i)
		}
		if plan.Funding == FundingV4Flash && (!step.Pool.IsV4() || step.Pool.Address != plan.Steps[0].Pool.Address) {
			return fmt.Errorf("step %d: V4 flash accounting requires only V4 pools on one manager", i)
		}
		native := step.TokenIn == (domain.Address{}) || step.TokenOut == (domain.Address{})
		if step.Pool.Kind == domain.PoolKindWrap {
			// The leg deposits into or withdraws from its target, the wrapped
			// native token, so that token is its other side. PriceCheck reads
			// no pool for it: a bound there would pass unchecked.
			if !native || (step.TokenIn != step.Pool.Address && step.TokenOut != step.Pool.Address) {
				return fmt.Errorf("step %d: a wrap pairs native currency with its wrapped token", i)
			}
			if step.Fee != 0 || step.TickSpacing != 0 || step.PriceBound != nil || step.SqrtPriceLimitX96 != nil && step.SqrtPriceLimitX96.Sign() != 0 {
				return fmt.Errorf("step %d: a wrap takes no fee, tick spacing, price limit or bound", i)
			}
		} else if !step.Pool.IsV4() && native {
			return fmt.Errorf("step %d: native currency requires V4 or a wrap", i)
		}
		if step.Fee >= 1<<24 || (step.Pool.Kind == domain.PoolKindV2 && step.Fee >= 1_000_000) || step.TickSpacing < -(1<<23) || step.TickSpacing >= 1<<23 {
			return fmt.Errorf("step %d: fee or tick spacing exceeds its field", i)
		}
		if err := validUint("step min out", step.MinAmountOut, 256, false); err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		if step.SqrtPriceLimitX96 != nil {
			if err := validUint("step price limit", step.SqrtPriceLimitX96, 160, false); err != nil {
				return fmt.Errorf("step %d: %w", i, err)
			}
			if (step.Pool.Kind == domain.PoolKindV2 || step.Pool.Kind == domain.PoolKindVelodromeV2) && step.SqrtPriceLimitX96.Sign() != 0 {
				return fmt.Errorf("step %d: V2 has no sqrt price limit", i)
			}
		}
		if step.PriceBound != nil {
			if err := validUint("step price bound", step.PriceBound, 160, true); err != nil {
				return fmt.Errorf("step %d: %w", i, err)
			}
		}
		// The executor rejects a MetricOmm leg with a limit (the pool's own
		// limit is a Q64.64 price, not a sqrt price), and PriceCheck reads no
		// MetricOmm pool: a bound there would pass unchecked.
		if step.Pool.Kind == domain.PoolKindMetricOmm && (step.PriceBound != nil || step.SqrtPriceLimitX96 != nil && step.SqrtPriceLimitX96.Sign() != 0) {
			return fmt.Errorf("step %d: MetricOmm takes no price limit or bound", i)
		}
		token = step.TokenOut
	}
	if token != plan.Asset {
		return fmt.Errorf("route does not close in its funding asset")
	}
	if plan.MinRate != nil && (plan.MinRate.Sign() <= 0 || plan.MinRate.Cmp(rateOne) >= 0) {
		return fmt.Errorf("min rate must be within (0, 1e18)")
	}
	return nil
}

func validUint(name string, value *big.Int, bits int, positive bool) error {
	if value == nil || value.Sign() < 0 || value.BitLen() > bits || (positive && value.Sign() == 0) {
		return fmt.Errorf("%s must fit uint%d with the required sign", name, bits)
	}
	return nil
}

func putUint64(word []byte, value uint64) { binary.BigEndian.PutUint64(word[24:32], value) }

// putInt24 writes a two's-complement ABI word for an int24 value.
func putInt24(word []byte, value int32) {
	fill := byte(0)
	if value < 0 {
		fill = 0xff
	}
	for i := range word[:28] {
		word[i] = fill
	}
	binary.BigEndian.PutUint32(word[28:32], uint32(value))
}

func keccak(encoded []byte) domain.Hash {
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write(encoded)
	var result domain.Hash
	copy(result[:], hasher.Sum(nil))
	return result
}
