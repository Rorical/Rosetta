package discovery

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// scanState is the per-call working set shared by the source scans.
type scanState struct {
	stats  Stats
	quotes map[domain.Address]struct{}
	seen   map[domain.PoolKey]struct{}
}

// restrictedTokens reports whether QuoteTokens limits which pairs are
// considered. An empty map is global discovery.
func (s *scanState) restrictedTokens() bool {
	return len(s.quotes) > 0
}

// candidate is a pool that passed the static filters, with the liquidity read
// attached once it is known.
type candidate struct {
	spec      Spec
	liquidity *big.Int
}

func (d *Discoverer) scanSource(ctx context.Context, source Source, from, to uint64, state *scanState) ([]*candidate, error) {
	return d.scanEmitter(ctx, Emitter{source, source.Contract(d.Config.Deployments)}, from, to, state)
}
func (d *Discoverer) scanEmitter(ctx context.Context, configured Emitter, from, to uint64, state *scanState) ([]*candidate, error) {
	source, emitter := configured.Source, configured.Address
	topics := [][]domain.Hash{{source.topic0()}}
	if source == SourceUniswapV4 {
		// Position 1 is the pool ID. A nil position matches every ID.
		topics = append(topics, nil)
	}

	var found []*candidate
	for start := from; ; {
		end := start + d.Config.ScanChunkBlocks - 1
		if end < start || end > to {
			end = to
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		logs, err := d.Logs.GetLogs(ctx, rpc.LogFilter{
			FromBlock: start,
			ToBlock:   end,
			Addresses: []domain.Address{emitter},
			Topics:    topics,
		})
		if err != nil {
			return nil, fmt.Errorf("discovery: %s logs %d-%d: %w", source, start, end, err)
		}
		state.stats.ScannedBlocks += end - start + 1

		for _, log := range logs {
			state.stats.Logs++
			if log.Address != emitter {
				state.stats.SkippedInvalid++
				continue
			}
			spec, ok, err := decodeSpec(source, log)
			if err != nil || !ok {
				state.stats.SkippedInvalid++
				continue
			}
			state.stats.Decoded++
			if !d.accept(spec, state) {
				continue
			}
			key := spec.PoolKey()
			if _, duplicate := state.seen[key]; duplicate {
				continue
			}
			state.seen[key] = struct{}{}
			found = append(found, &candidate{spec: spec})
		}

		if end == to {
			break
		}
		start = end + 1
	}
	return found, nil
}

func decodeSpec(source Source, log rpc.Log) (Spec, bool, error) {
	switch source {
	case SourceVelodromeV2:
		return DecodeVelodromePoolCreated(log)
	case SourceUniswapV3, SourcePancakeV3:
		return DecodeV3PoolCreated(log)
	case SourceSlipstream:
		return DecodeSlipstreamPoolCreated(log)
	case SourceUniswapV4:
		return DecodeV4Initialize(log)
	case SourceUniswapV2:
		return DecodeV2PairCreated(log)
	case SourceCamelotV3:
		return DecodeCamelotPool(log)
	case SourceSolidlyV3:
		return DecodeSolidlyV3PoolCreated(log)
	case SourceVelodromeV1:
		return DecodeVelodromeV1PairCreated(log)
	default:
		return Spec{}, false, fmt.Errorf("discovery: unknown source %d", source)
	}
}

// DecodeCreation checks the configured emitter and signature before decoding.
// It does not perform discovery filters or read current pool state.
func DecodeCreation(source Source, log rpc.Log, deployments ...*domain.Deployments) (Spec, bool, error) {
	if !source.Valid() {
		return Spec{}, false, fmt.Errorf("discovery: unknown source %d", source)
	}
	if !source.Matches(log.Address, log.Topics, deployments...) {
		return Spec{}, false, nil
	}
	return decodeSpec(source, log)
}

// accept applies the filters that need no contract read.
func (d *Discoverer) accept(spec Spec, state *scanState) bool {
	if state.restrictedTokens() {
		if _, ok := state.quotes[spec.Token0]; !ok {
			state.stats.SkippedToken++
			return false
		}
		if _, ok := state.quotes[spec.Token1]; !ok {
			state.stats.SkippedToken++
			return false
		}
	}
	if spec.Address == (domain.Address{}) || spec.Token1 == (domain.Address{}) || spec.Token0 == spec.Token1 {
		state.stats.SkippedInvalid++
		return false
	}
	if spec.Kind != domain.PoolKindV4 && spec.Token0 == (domain.Address{}) {
		state.stats.SkippedInvalid++
		return false
	}
	if spec.Kind.UsesTicks() && spec.Kind != domain.PoolKindCamelotV3 && spec.TickSpacing <= 0 {
		state.stats.SkippedInvalid++
		return false
	}

	if spec.Kind == domain.PoolKindV4 {
		if spec.Hooks != (domain.Address{}) {
			state.stats.SkippedHooks++
			return false
		}
		expected, err := uniswapv4.PoolID(spec.Token0, spec.Token1, spec.Fee, spec.TickSpacing, spec.Hooks)
		if err != nil || expected != spec.PoolID {
			state.stats.SkippedInvalid++
			return false
		}
	} else if spec.Kind != domain.PoolKindSlipstream && spec.Kind != domain.PoolKindCamelotV3 && spec.Kind != domain.PoolKindVelodromeV2 && spec.Fee == 0 {
		// Slipstream's event has no fee; it is read and checked in readFees.
		state.stats.SkippedInvalid++
		return false
	}

	if spec.Kind != domain.PoolKindSlipstream && spec.Kind != domain.PoolKindCamelotV3 && spec.Fee > d.Config.MaxFee {
		state.stats.SkippedFee++
		return false
	}
	return true
}
