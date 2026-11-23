package uniswapv4

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestFilterLog(t *testing.T) {
	poolA := domain.Hash{0xaa}
	poolB := domain.Hash{0xbb}
	manager := domain.Address{0x05}
	other := domain.Address{0x06}
	tracked := map[domain.Hash]struct{}{poolA: {}}
	tests := []struct {
		name       string
		poolIDs    map[domain.Hash]struct{}
		logAddress domain.Address
		topics     []domain.Hash
		want       bool
	}{
		{
			name:       "tracked pool",
			poolIDs:    tracked,
			logAddress: manager,
			topics:     []domain.Hash{{0x40}, poolA},
			want:       true,
		},
		{
			name:       "untracked pool",
			poolIDs:    tracked,
			logAddress: manager,
			topics:     []domain.Hash{{0x40}, poolB},
			want:       false,
		},
		{
			name:       "wrong log address",
			poolIDs:    tracked,
			logAddress: other,
			topics:     []domain.Hash{{0x40}, poolA},
			want:       false,
		},
		{
			name:       "no topics",
			poolIDs:    tracked,
			logAddress: manager,
			topics:     nil,
			want:       false,
		},
		{
			name:       "only topic0",
			poolIDs:    tracked,
			logAddress: manager,
			topics:     []domain.Hash{{0x40}},
			want:       false,
		},
		{
			name:       "nil pool set",
			poolIDs:    nil,
			logAddress: manager,
			topics:     []domain.Hash{{0x40}, poolA},
			want:       false,
		},
		{
			name:       "pool id in later topic is not matched",
			poolIDs:    tracked,
			logAddress: manager,
			topics:     []domain.Hash{{0x40}, poolB, poolA},
			want:       false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := FilterLog(test.poolIDs, manager, test.logAddress, test.topics)
			if got != test.want {
				t.Fatalf("FilterLog = %t, want %t", got, test.want)
			}
		})
	}
}
