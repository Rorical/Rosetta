package discovery

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type Emitter struct {
	Source  Source
	Address domain.Address
}
type emitterJSON struct {
	Source  string `json:"source"`
	Address string `json:"address"`
}
type Creation struct {
	Verified bool
	Spec     Spec
	Emitter  domain.Address
	Source   Source
	Block    uint64
}

func (c Config) Emitters() []Emitter {
	out := []Emitter{}
	for _, source := range c.Sources {
		out = append(out, Emitter{source, source.Contract(c.Deployments)})
	}
	return append(out, c.AdditionalEmitters...)
}
func (e Emitter) Matches(address domain.Address, topics []domain.Hash) bool {
	return address == e.Address && e.Address != (domain.Address{}) && len(topics) > 0 && topics[0] == e.Source.topic0()
}

// ScanCreations reads and statically filters metadata without liquidity/tick RPC.
// This lets background owners apply activity and admission budgets before getters.
func (d *Discoverer) ScanCreations(ctx context.Context, from, to uint64) ([]Creation, error) {
	if from > to {
		return nil, fmt.Errorf("discovery: reversed creation range")
	}
	state := &scanState{quotes: map[domain.Address]struct{}{}, seen: map[domain.PoolKey]struct{}{}}
	for _, token := range d.Config.QuoteTokens {
		state.quotes[token] = struct{}{}
	}
	var result []Creation
	for _, emitter := range d.Config.Emitters() {
		for start := from; ; {
			end := min(to, start+d.Config.ScanChunkBlocks-1)
			if end < start {
				end = to
			}
			logs, err := d.Logs.GetLogs(ctx, rpc.LogFilter{FromBlock: start, ToBlock: end, Addresses: []domain.Address{emitter.Address}, Topics: [][]domain.Hash{{emitter.Source.topic0()}}})
			if err != nil {
				return nil, err
			}
			for _, log := range logs {
				if !emitter.Matches(log.Address, log.Topics) {
					continue
				}
				spec, ok, err := decodeSpec(emitter.Source, log)
				if err != nil || !ok || !d.accept(spec, state) {
					continue
				}
				key := spec.PoolKey()
				if _, seen := state.seen[key]; seen {
					continue
				}
				state.seen[key] = struct{}{}
				result = append(result, Creation{Spec: spec, Emitter: emitter.Address, Source: emitter.Source, Block: log.BlockNumber})
			}
			if end == to {
				break
			}
			start = end + 1
		}
	}
	return result, nil
}
