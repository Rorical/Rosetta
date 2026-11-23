package discovery

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

var (
	camelotGlobalCall  = []byte{0xe7, 0x6c, 0x01, 0xe4}
	camelotSpacingCall = []byte{0xd0, 0xc9, 0x3a, 0x7c}
)

// The creation event has neither mutable spacing nor directional fees. Read
// both at the sizing snapshot; an invalid pool is isolated from other candidates.
func (d *Discoverer) readCamelotMetadata(ctx context.Context, candidates []*candidate, block uint64, stats *Stats) ([]*candidate, error) {
	var targets []*candidate
	var calls []pool.ContractCall
	for _, c := range candidates {
		if c.spec.Kind != domain.PoolKindCamelotV3 {
			continue
		}
		targets = append(targets, c)
		for _, data := range [][]byte{camelotGlobalCall, camelotSpacingCall} {
			calls = append(calls, pool.ContractCall{To: c.spec.Address, Data: data, Block: fmt.Sprintf("0x%x", block)})
		}
	}
	outputs, failures, err := d.callRound(ctx, calls)
	if err != nil {
		return nil, fmt.Errorf("discovery: Camelot metadata: %w", err)
	}
	drop := make(map[*candidate]struct{})
	for i, c := range targets {
		failed := false
		for _, failure := range failures[2*i : 2*i+2] {
			if failure == nil {
				continue
			}
			if !isCallError(failure) {
				return nil, fmt.Errorf("discovery: Camelot metadata: %w", failure)
			}
			failed = true
		}
		if failed {
			stats.SkippedInvalid++
			drop[c] = struct{}{}
			continue
		}
		metadata, err := camelotv3.ParseMetadata(outputs[2*i], outputs[2*i+1])
		if err != nil {
			stats.SkippedInvalid++
			drop[c] = struct{}{}
			continue
		}
		if max(metadata.Fee, metadata.FeeOneForZero) > d.Config.MaxFee {
			stats.SkippedFee++
			drop[c] = struct{}{}
			continue
		}
		c.spec.Fee, c.spec.TickSpacing = metadata.Fee, metadata.TickSpacing
	}
	return without(candidates, drop), nil
}
