package discovery

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A pool already in the creation records but not in the catalog was probed
// again every round under a kind-less activity key, then reported as an
// unconfigured factory. It must keep its selection reason and spend no probe.
func TestProbeSkipsPoolsAlreadyInCreationRecords(t *testing.T) {
	address := domain.Address{0x95}
	recorded := Creation{Spec: Spec{Address: address, Kind: domain.PoolKindV3, Token0: domain.Address{1}, Token1: domain.Address{2}, Fee: 100, TickSpacing: 1}, Source: SourceUniswapV3}
	activity := domain.PoolKey{Address: address}
	caller := newFakeCaller()
	options := ProbeOptions{
		Metadata: []Creation{recorded},
		Activity: map[domain.PoolKey]Activity{activity: {Swaps: 155}},
		Seen:     map[domain.PoolKey]bool{recorded.Spec.PoolKey(): true},
		Limit:    4,
		Anchor:   domain.BlockRef{Number: 100},
		Cooldown: 1,
	}
	probe := ActiveProbe{}
	rows, err := probe.RunWithLogs(t.Context(), caller, nil, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(caller.calls) != 0 || len(probe.Report) != 0 {
		t.Fatal("recorded pool was probed again", rows, caller.calls, probe.Report)
	}
}
