package remote

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

type paperExecutionResult struct {
	TransactionQuote *arb.Opportunity            `json:"transaction_quote,omitempty"`
	PoolFees         []execution.ReactivePoolFee `json:"transaction_pool_fees,omitempty"`

	DiscoveryCanonicalBlock *domain.BlockRef               `json:"discovery_canonical_block,omitempty"`
	FeeDiagnostic           *paperFeeDiagnostic            `json:"fee_diagnostic,omitempty"`
	Selection               *paperSelection                `json:"selection,omitempty"`
	Bidding                 *paperBidBudget                `json:"bidding,omitempty"`
	Competition             *paperCompetition              `json:"competition,omitempty"`
	RPCCalls                *uint64                        `json:"rpc_calls,omitempty"`
	CanonicalTraceBlock     *domain.BlockRef               `json:"canonical_trace_block,omitempty"`
	SnapshotNS              int64                          `json:"snapshot_ns,omitempty"`
	FeePrefetchNS           int64                          `json:"fee_prefetch_ns,omitempty"`
	SetupNS                 int64                          `json:"setup_ns,omitempty"`
	ExecutionNS             int64                          `json:"execution_ns,omitempty"`
	PlannedDelaysMS         []int64                        `json:"planned_delays_ms,omitempty"`
	QueuedAtNS              int64                          `json:"queued_at_ns,omitempty"`
	Group                   domain.Hash                    `json:"group_id"`
	DelayMS                 int64                          `json:"delay_ms"`
	ActualDelayNS           int64                          `json:"actual_delay_ns,omitempty"`
	ObservedAtNS            int64                          `json:"observed_at_ns,omitempty"`
	ExecutionVersion        domain.StateVersion            `json:"execution_version"`
	StateBasis              string                         `json:"state_basis,omitempty"`
	Candidate               *arb.Opportunity               `json:"candidate,omitempty"`
	Type                    string                         `json:"type"`
	Schema                  int                            `json:"schema"`
	Basis                   string                         `json:"basis"`
	Chain                   domain.ChainIdentity           `json:"chain"`
	ID                      domain.Hash                    `json:"sample_id"`
	Artifact                domain.Hash                    `json:"creation_code_hash"`
	Version                 domain.StateVersion            `json:"version"`
	Flash                   *execution.FlashblocksEvidence `json:"flashblocks,omitempty"`
	DiscoveredAtNS          int64                          `json:"discovered_at_ns"`
	FeedReceivedAtNS        int64                          `json:"feed_received_at_ns"`
	StartedAtNS             int64                          `json:"started_at_ns"`
	FinishedAtNS            int64                          `json:"finished_at_ns"`
	Status                  string                         `json:"status"`
	Error                   string                         `json:"error,omitempty"`
	RevertData              string                         `json:"revert_data,omitempty"`
	Route                   *execution.ExecutorPlan        `json:"route,omitempty"`
	Call                    *execution.ExecutorCall        `json:"call,omitempty"`
	QuotedGross             *big.Int                       `json:"quoted_gross,omitempty"`
	Profit                  *big.Int                       `json:"profit_after_loan,omitempty"`
	LoanFee                 *big.Int                       `json:"loan_fee,omitempty"`
	GasUsed                 uint64                         `json:"gas_used,omitempty"`
	TotalFee                *big.Int                       `json:"total_fee_native_upper_bound,omitempty"`
	Evidence                *execution.ExecutionEvidence   `json:"evidence,omitempty"`
	Economics               *pipeline.ResearchEconomics    `json:"economics,omitempty"`
	Intent                  *paperIntentReference          `json:"reactive_intent,omitempty"`
}

// executorRevert preserves measured gas and return data without turning a
// failed transaction into confirmation evidence or a dispatchable plan.
type executorRevert struct {
	Gas    uint64
	Data   string
	Reason string
}

func (e *executorRevert) Error() string { return "OP executor failed: " + e.Reason }

func (s *snapshot) executePaper(ctx context.Context, request execution.ConfirmationRequest, result *paperExecutionResult) error {
	call, err := execution.BuildExecutorCall(ctx, request.Plan, request.Parameters)
	if err != nil {
		return err
	}
	result.Route, result.Call = &request.Plan, &call
	proof, err := s.confirmOP(ctx, request)
	if err == nil {
		result.Status = "success"
		result.Evidence = proof
		result.Profit, result.LoanFee, result.GasUsed, result.TotalFee = proof.Profit, proof.LoanFee, proof.GasUsed, proof.TotalFeeWei
		return nil
	}
	var reverted *executorRevert
	if !errors.As(err, &reverted) {
		return err
	}
	result.Status = "reverted"
	result.Error, result.RevertData, result.GasUsed = reverted.Error(), reverted.Data, reverted.Gas
	// Keep failures even if the fee oracle becomes unavailable. Unknown cost
	// never becomes a zero-cost loss or a successful execution sample.
	quote, feeErr := s.quoteOPFees(ctx, call)
	if feeErr != nil {
		return fmt.Errorf("revert fee quotation: %w", feeErr)
	}
	operator, feeErr := s.opFeeWord(ctx, "getOperatorFee(uint256)", reverted.Gas)
	if feeErr != nil {
		return feeErr
	}
	price := new(big.Int).Add(s.block.BaseFee, call.TipCap)
	result.TotalFee = new(big.Int).Mul(new(big.Int).SetUint64(reverted.Gas), price)
	result.TotalFee.Add(result.TotalFee, quote.proof.L1FeeUpperBound).Add(result.TotalFee, operator)
	return s.checkCanonical(ctx)
}
