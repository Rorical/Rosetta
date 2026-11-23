package remote

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
)

type paperSetupCache struct {
	executor domain.Address
	overlay  map[string]any
}

// Only virtual executor/operator state is reusable. Never cache simulated
// swaps, deposits, approvals, token state or any real protocol storage. The
// executor keeps no pool registry, so one setup serves every route.
func (p *executionPaper) setup(ctx context.Context, s *snapshot, sender domain.Address) (domain.Address, error) {
	encoded, _ := json.Marshal([]any{p.artifact.hash, p.chain, sender})
	key := digest(encoded)
	if saved, ok := p.setups[key]; ok {
		code, err := s.CodeHash(ctx, saved.executor)
		if err != nil || code != (domain.Hash{}) {
			return domain.Address{}, fmt.Errorf("paper executor address occupied or unavailable")
		}
		s.overlay = mergeOverrides(s.overlay, saved.overlay)
		return saved.executor, nil
	}
	executor, err := s.setupPaperExecutor(ctx, p.artifact, sender)
	if err != nil {
		return executor, err
	}
	if p.setups == nil {
		p.setups = make(map[domain.Hash]paperSetupCache)
	}
	overlay := mergeOverrides(nil, map[string]any{sender.Hex(): s.overlay[sender.Hex()], executor.Hex(): s.overlay[executor.Hex()]})
	p.setups[key] = paperSetupCache{executor: executor, overlay: overlay}
	return executor, nil
}
