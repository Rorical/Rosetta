package remote

import (
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestFeeScheduleRereadsSetterFeesOnlyWhenTheyCanMove(t *testing.T) {
	velodrome := bootstrap.Spec{Address: domain.Address{1}, Kind: domain.PoolKindVelodromeV2}
	v4 := bootstrap.Spec{Address: domain.Address{2}, Kind: domain.PoolKindV4, PoolID: domain.Hash{2}}
	hooked := bootstrap.Spec{Address: domain.Address{2}, Kind: domain.PoolKindV4, PoolID: domain.Hash{3}, Hooks: domain.Address{9}}
	slip := bootstrap.Spec{Address: domain.Address{4}, Kind: domain.PoolKindSlipstream}
	camelot := bootstrap.Spec{Address: domain.Address{5}, Kind: domain.PoolKindCamelotV3}
	specs := []bootstrap.Spec{velodrome, v4, hooked, slip, camelot}
	// Camelot's own Fee event updates the store; it is read like a setter fee.
	always := []bootstrap.Spec{hooked, slip}
	schedule := feeSchedule{every: 1800} // an hour of OP blocks
	read := func(block uint64, transactions ...ingest.Transaction) []bootstrap.Spec {
		due := schedule.due(specs, block, transactions)
		var states []pool.State
		for _, spec := range due {
			states = append(states, pool.State{Key: domain.PoolKey{Address: spec.Address, Kind: spec.Kind, PoolID: spec.PoolID, Hooks: spec.Hooks}})
		}
		schedule.mark(states, block)
		return due
	}
	event := func(topic domain.Hash) ingest.Transaction {
		return ingest.Transaction{Logs: []ingest.Log{{Address: domain.Address{0xfa}, Topics: []domain.Hash{topic, {1}}}}}
	}
	swap := ingest.Transaction{Logs: []ingest.Log{{Address: velodrome.Address, Topics: []domain.Hash{{0x5a}}}}}
	for _, step := range []struct {
		name  string
		block uint64
		txs   []ingest.Transaction
		want  []bootstrap.Spec
	}{
		{"first sight", 100, nil, specs},
		{"next block", 101, []ingest.Transaction{swap}, always},
		{"Velodrome custom fee", 102, []ingest.Transaction{event(velodromeCustomFeeTopic)}, specs},
		{"after the event", 103, nil, always},
		{"V4 protocol fee", 104, []ingest.Transaction{event(v4ProtocolFeeTopic)}, specs},
		{"before the interval", 104 + schedule.every - 1, nil, always},
		{"interval", 104 + schedule.every, nil, specs},
		{"older block after a rewind", 120, nil, specs},
	} {
		if got := read(step.block, step.txs...); !slices.Equal(got, step.want) {
			t.Fatalf("%s: read %v, want %v", step.name, got, step.want)
		}
	}
	// A pool adopted later has no read yet.
	added := bootstrap.Spec{Address: domain.Address{6}, Kind: domain.PoolKindVelodromeV2}
	if got := schedule.due(append(specs, added), 121, nil); !slices.Equal(got, append(slices.Clone(always), added)) {
		t.Fatalf("new pool not read: %v", got)
	}
}
