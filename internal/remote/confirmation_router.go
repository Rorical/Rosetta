package remote

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

func (s *snapshot) confirm(ctx context.Context, request execution.ConfirmationRequest) (*execution.ExecutionEvidence, error) {
	switch s.chain.Family {
	case domain.FamilyNitro:
		return s.confirmNitro(ctx, request)
	case domain.FamilyOPStack:
		return s.confirmOP(ctx, request)
	default:
		return nil, fmt.Errorf("unsupported remote execution family %q", s.chain.Family)
	}
}
