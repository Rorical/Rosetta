package pipeline

import "testing"

func TestExploreLimitsCachePaths(t *testing.T) {
	base := exploreLimits(4, 0)
	if base.CachePaths != 8192 {
		t.Fatalf("zero cache paths = %d, want 8192", base.CachePaths)
	}
	configured := exploreLimits(4, 32768)
	if configured.CachePaths != 32768 {
		t.Fatalf("configured cache paths = %d, want 32768", configured.CachePaths)
	}
	if configured.MaxDepth != base.MaxDepth || configured.Steps != base.Steps || configured.NewPaths != base.NewPaths {
		t.Fatalf("cache cap changed other explore limits: %+v, want %+v", configured, base)
	}
}
