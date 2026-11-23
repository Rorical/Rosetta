package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ReadyPlan contains the exact unsigned invocation and its execution proof.
// Consumers must call ValidateForDispatch with current state immediately before
// using it. JSON consumers must retain full integer precision and decode Data
// using the standard encoding/json []byte base64 representation.
type ReadyPlan struct {
	SchemaVersion uint32
	ID            domain.Hash
	Status        string
	Version       domain.StateVersion
	GraphVersion  uint64
	PolicyHash    domain.Hash
	Route         ExecutorPlan
	Evidence      ExecutionEvidence
	Economics     Economics
	FastReplay    *FastReplayEvidence  `json:",omitempty"`
	Flashblocks   *FlashblocksEvidence `json:",omitempty"`
	CreatedUnixMS int64
	ExpiresUnixMS int64
}

// SealReadyPlan takes ownership of its fields. Any subsequent mutation breaks
// its ID and fails the dispatch check.
func SealReadyPlan(ctx context.Context, plan ReadyPlan, models ...FeeModel) (*ReadyPlan, error) {
	plan.SchemaVersion, plan.Status = 1, "ready"
	plan.ID = domain.Hash{}
	if err := validatePlan(ctx, &plan, models...); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("encode execution plan: %w", err)
	}
	plan.ID = keccak(encoded)
	return &plan, nil
}

func ValidateForDispatch(ctx context.Context, plan *ReadyPlan, current domain.StateVersion, chainID uint64, graph uint64, policy domain.Hash, nowUnixMS int64, models ...FeeModel) error {
	if current.Source != domain.SourceStandard {
		return fmt.Errorf("nonstandard plans require current Fast stream and context-source observations")
	}
	return validateForDispatch(ctx, plan, current, chainID, graph, policy, nowUnixMS, models...)
}

func validateForDispatch(ctx context.Context, plan *ReadyPlan, current domain.StateVersion, chainID uint64, graph uint64, policy domain.Hash, nowUnixMS int64, models ...FeeModel) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan == nil || chainID == 0 || plan.Evidence.Context.ChainID != chainID || plan.Version != current || plan.GraphVersion != graph || plan.PolicyHash != policy ||
		nowUnixMS < plan.CreatedUnixMS || nowUnixMS >= plan.ExpiresUnixMS {
		return fmt.Errorf("execution plan is stale, expired or belongs to another policy")
	}
	if err := validatePlan(ctx, plan, models...); err != nil {
		return err
	}
	copy := *plan
	copy.ID = domain.Hash{}
	encoded, err := json.Marshal(copy)
	if err != nil || keccak(encoded) != plan.ID {
		return fmt.Errorf("execution plan was modified after confirmation")
	}
	return nil
}

func validatePlan(ctx context.Context, plan *ReadyPlan, models ...FeeModel) error {
	if plan.SchemaVersion != 1 || plan.Status != "ready" || plan.GraphVersion == 0 || plan.PolicyHash == (domain.Hash{}) ||
		plan.Version.Anchor.Hash == (domain.Hash{}) || plan.Version.PrefixHash == (domain.Hash{}) ||
		plan.CreatedUnixMS <= 0 || plan.ExpiresUnixMS <= plan.CreatedUnixMS || plan.ExpiresUnixMS-plan.CreatedUnixMS > 30_000 {
		return fmt.Errorf("invalid execution-plan identity or validity window")
	}
	if plan.Evidence.Context.ChainID == 0 || plan.Evidence.GasUsed == 0 ||
		plan.Evidence.Call.Nonce == ^uint64(0) || plan.Evidence.NonceAfter != plan.Evidence.Call.Nonce+1 || plan.Route.Deadline > uint64(^uint64(0)>>1)/1000 ||
		plan.Route.Deadline <= plan.Evidence.Context.Timestamp || plan.Route.Deadline-plan.Evidence.Context.Timestamp > 30 ||
		plan.ExpiresUnixMS > int64(plan.Route.Deadline)*1000 {
		return fmt.Errorf("invalid execution context, nonce or deadline")
	}
	if err := validatePlanSource(plan); err != nil {
		return err
	}
	call, err := BuildExecutorCall(ctx, plan.Route, plan.Evidence.Call.CallParameters)
	if err != nil {
		return err
	}
	if !bytes.Equal(call.Data, plan.Evidence.Call.Data) || plan.Evidence.Call.Value == nil || call.Value.Cmp(plan.Evidence.Call.Value) != 0 {
		return fmt.Errorf("execution evidence differs from route calldata or value")
	}
	model, err := feeModel(plan.Evidence, models)
	if err != nil {
		return err
	}
	if err := model.Validate(plan.Evidence); err != nil {
		return err
	}
	economics, err := CalculateEconomics(plan.Evidence, plan.Economics.Conversion, plan.Economics.MinimumNetProfit, model)
	if err != nil {
		return err
	}
	if economics.BudgetNetProfit.Cmp(economics.MinimumNetProfit) < 0 ||
		plan.Route.MinProfit.Cmp(new(big.Int).Add(economics.MinimumNetProfit, economics.BudgetInAsset)) < 0 {
		return fmt.Errorf("execution profit does not cover fee budget and required net profit")
	}
	if plan.Evidence.Profit.Cmp(plan.Route.MinProfit) < 0 || plan.Evidence.LoanFee == nil || plan.Evidence.LoanFee.Sign() < 0 ||
		plan.Evidence.LoanFee.Cmp(plan.Route.MaxLoanFee) > 0 || plan.Evidence.GasUsedForL1 > plan.Evidence.GasUsed {
		return fmt.Errorf("execution evidence violates contract or gas bounds")
	}
	if err := validateConversion(plan); err != nil {
		return err
	}
	want, _ := json.Marshal(economics)
	got, _ := json.Marshal(plan.Economics)
	if !bytes.Equal(want, got) {
		return fmt.Errorf("execution-plan economics differ from confirmed values")
	}
	return nil
}
