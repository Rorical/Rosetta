package remote

import "github.com/Rorical/Rosetta/internal/domain"

// Only the serial worker owns this proof. A feed payload's parent claim cannot
// prove that it descends from the exact discovery prefix that was simulated.
type paperDelayBaseline struct {
	group domain.Hash
	block *domain.BlockRef
}

func (b *paperDelayBaseline) observe(result paperExecutionResult) {
	b.group, b.block = result.Group, nil
	if result.StateBasis == "discovery_snapshot" && result.CanonicalTraceBlock != nil {
		ref := *result.CanonicalTraceBlock
		b.block = &ref
	}
}

func (b *paperDelayBaseline) bind(job *paperExecutionJob) {
	if !job.crossBlock || job.censored {
		return
	}
	if b.group != job.group || b.block == nil || b.block.Hash == (domain.Hash{}) {
		job.censored, job.unavailable = true, "cross-block delay lacks verified discovery block"
		return
	}
	if job.prefix == nil || job.prefix.Base.Number <= b.block.Number || job.prefix.Base.Number-b.block.Number != 1 || job.prefix.Base.Parent != b.block.Hash {
		job.censored, job.unavailable = true, "later prefix does not descend from verified discovery block"
		return
	}
	ref := *b.block
	job.discoveryBlock = &ref
}
