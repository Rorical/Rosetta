package remote

import (
	"context"
	"encoding/json"

	"github.com/Rorical/Rosetta/internal/rpc"
)

func (s *snapshot) readCache() *rpc.ReadCache {
	if reads := s.reads.Load(); reads != nil {
		return reads
	}
	reads := &rpc.ReadCache{}
	if s.reads.CompareAndSwap(nil, reads) {
		return reads
	}
	return s.reads.Load()
}
func (s *snapshot) readString(ctx context.Context, method string, params []any) (string, error) {
	if err := s.guard(ctx); err != nil {
		return "", err
	}
	encoded, err := json.Marshal([]any{method, params})
	if err != nil {
		return "", err
	}
	return s.readCache().Load(ctx, string(encoded), func(ctx context.Context) (string, error) {
		var value string
		err := s.client.Call(ctx, method, params, &value)
		return value, err
	})
}
