package remote

import "context"

// postState reconstructs the same executor invocation as callTracer. Its base
// includes the fixed Flashblocks prefix state when the snapshot is tentative.
func (s *snapshot) postState(ctx context.Context, args map[string]any, initial map[string]any) (map[string]any, error) {
	var diff stateDiff
	options := map[string]any{"tracer": "prestateTracer", "tracerConfig": map[string]any{"diffMode": true}, "timeout": "1s"}
	if initial != nil {
		options["stateOverrides"] = initial
	}
	if err := s.traceCall(ctx, args, options, &diff); err != nil {
		return nil, err
	}
	patch, err := diff.overrides()
	if err != nil {
		return nil, err
	}
	return mergeOverrides(mergeOverrides(s.overlay, initial), patch), nil
}
