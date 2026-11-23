package pipeline

import (
	"context"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

type swapAuditJob struct {
	base         poolstate.View
	ref          domain.BlockRef
	transactions []ingest.Transaction
	fees         []pool.State
}

// ObserveReceipts keeps canonical fee evidence contiguous even while historical
// search and expensive quote audits are paused during catch-up. No RPC is read.
func (r *Research) ObserveReceipts(base poolstate.View, ref domain.BlockRef, transactions []ingest.Transaction) {
	if r != nil && r.book != nil {
		r.book.observeReceipts(base, ref, transactions)
	} else if r != nil {
		r.observeSpot(base, ref)
	}
}

// ObserveBlock accepts only canonical receipts and the preceding pool view.
// Transactions must remain immutable after submission, just like frame views.
// A bounded side queue keeps receipt replay and quote math off the live writer.
func (r *Research) ObserveBlock(base poolstate.View, ref domain.BlockRef, transactions []ingest.Transaction, fees ...pool.State) {
	if r == nil {
		return
	}
	r.ObserveReceipts(base, ref, transactions)
	if r.policy.SwapAudit.MaxSwaps == 0 || ref.Number%r.policy.SwapAudit.EveryBlocks != 0 {
		return
	}
	select {
	case r.auditJobs <- swapAuditJob{base: base, ref: ref, transactions: transactions, fees: fees}:
	default:
		r.auditSkipped.Add(1)
	}
}

func (r *Research) runSwapAudit(ctx context.Context) {
	defer close(r.auditDone)
	for {
		select {
		case <-ctx.Done():
			select {
			case <-r.auditJobs:
				r.auditSkipped.Add(1)
			default:
			}
			return
		case job := <-r.auditJobs:
			evaluation, cancel := context.WithTimeout(ctx, time.Duration(r.policy.SwapAudit.EvaluationMS)*time.Millisecond)
			result, err := AuditSwaps(evaluation, job.base, job.ref, job.transactions, r.policy.SwapAudit.MaxSwaps, job.fees...)
			cancel()
			if r.book != nil {
				r.book.observeQuotes(job.ref, result)
			}
			event := ResearchEvent{Type: "swap_quote_audit", AtMS: time.Now().UnixMilli(), Version: domain.StateVersion{Source: domain.SourceStandard, Anchor: job.ref, BlockNumber: job.ref.Number, PrefixHash: job.ref.Hash}, Details: result}
			if err != nil {
				event.Censored, event.Reason = true, err.Error()
			}
			select {
			case r.confirmations <- event:
			default:
				r.auditSkipped.Add(1)
			}
		}
	}
}
