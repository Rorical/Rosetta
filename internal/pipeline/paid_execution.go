package pipeline

import (
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// RecordPaidExecution feeds the actual executor receipt into the shared fee
// model. Canonical swap ingestion may already have recorded it; hash-based
// replacement prevents duplicate weighting. Reverts remain in the dispatch
// journal, since their partial execution cost is not a successful-route quote.
func (r *Research) RecordPaidExecution(sample *execution.Settlement) {
	if r == nil || r.book == nil || sample == nil || !sample.Success || sample.Cancelled || sample.Intent == nil || sample.Fees == nil || sample.Fees.Total == nil {
		return
	}
	b := r.book
	b.mu.Lock()
	defer b.mu.Unlock()
	header, ok := b.headers[sample.Block.Number]
	if b.pending || !ok || header.Hash != sample.Block.Hash {
		return
	}
	keys := make([]domain.PoolKey, len(sample.Intent.Route.Steps))
	for i, step := range sample.Intent.Route.Steps {
		keys[i] = step.Pool
	}
	if len(keys) < 2 || len(keys) > 16 {
		return
	}
	b.paid = slices.DeleteFunc(b.paid, func(s paidSample) bool { return s.tx == sample.Hash })
	// A fee receipt can name its block by number and hash alone; the book's
	// header is the full reference restore checks against.
	b.paid = append(b.paid, paidSample{shape: paperShape(keys), block: header, tx: sample.Hash, fees: sample.Fees})
	if len(b.paid) > 2048 {
		b.paid = slices.Clone(b.paid[len(b.paid)-2048:])
	}
	b.revision++
}
