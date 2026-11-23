package remote

import (
	"context"
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

func (s *snapshot) observeTransferFailure(ctx context.Context, args, options map[string]any) error {
	options["tracerConfig"] = map[string]any{"onlyTopCall": false}
	var frame tokenrisk.CallFrame
	if err := s.traceCall(ctx, args, options, &frame); err != nil {
		// A diagnostic outage cannot manufacture evidence. Preserve the original
		// execution rejection and allow another sample to diagnose it later.
		return nil
	}
	block := s.riskBlock
	if block.Hash == (domain.Hash{}) {
		block = s.block.Ref
	}
	executor, err := domain.ParseAddress(fmt.Sprint(args["to"]))
	if err != nil {
		return nil
	}
	for _, e := range tokenrisk.Detect(frame, block, executor) {
		if err := s.risks.Record(e); err != nil {
			return err
		}
	}
	return nil
}
