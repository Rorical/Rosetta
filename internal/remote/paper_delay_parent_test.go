package remote

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestPaperNextBlockRequiresVerifiedParentFromSameGroup(t *testing.T) {
	for _, fault := range []string{"", "wrong parent", "wrong group", "missing canonical proof", "failed baseline", "skipped block"} {
		t.Run(fault, func(t *testing.T) {
			r, original := delayFixture()
			block := domain.BlockRef{Number: 11, Hash: domain.Hash{11}}
			result := paperExecutionResult{Group: domain.Hash{1}, StateBasis: "discovery_snapshot", CanonicalTraceBlock: &block}
			prefix := r.flashPrefix.freeze()
			prefix.Base.Number, prefix.Base.Parent = 12, block.Hash
			job := original
			job.group, job.delayMS, job.crossBlock, job.prefix = result.Group, 200, true, &prefix
			switch fault {
			case "wrong parent":
				job.prefix.Base.Parent = domain.Hash{99}
			case "wrong group":
				job.group = domain.Hash{2}
			case "missing canonical proof":
				result.CanonicalTraceBlock = nil
			case "failed baseline":
				result.StateBasis = "unverified"
			case "skipped block":
				job.prefix.Base.Number++
			}
			var baseline paperDelayBaseline
			baseline.observe(result)
			baseline.bind(&job)
			if fault == "" {
				if job.censored || job.discoveryBlock == nil || *job.discoveryBlock != block {
					t.Fatal("valid descendant lost its canonical evidence")
				}
				block.Hash = domain.Hash{77}
				if job.discoveryBlock.Hash != (domain.Hash{11}) {
					t.Fatal("baseline evidence aliases mutable state")
				}
				return
			}
			if !job.censored || job.discoveryBlock != nil {
				t.Fatal("unverified branch admitted", fault)
			}
			if r.executionPaper.evaluate(t.Context(), job).Status != "censored" {
				t.Fatal("unverified parent triggered execution")
			}
		})
	}
}
