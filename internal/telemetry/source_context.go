package telemetry

import "context"

type sourceKey struct{}

// WithSource labels backend work without exposing orchestration context keys.
func WithSource(ctx context.Context, source Source) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}
func SourceFromContext(ctx context.Context) Source {
	source, _ := ctx.Value(sourceKey{}).(Source)
	return source
}
