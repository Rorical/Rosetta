package discovery

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// DecodeVelodromePoolCreated decodes the indexed stable flag separately from
// the address and pool-count data. The effective fee is not in the event.
func DecodeVelodromePoolCreated(log rpc.Log) (Spec, bool, error) {
	if !hasTopic0(log.Topics, topicVelodromePoolCreated) {
		return Spec{}, false, nil
	}
	if len(log.Topics) != 4 || len(log.Data) != 64 || new(big.Int).SetBytes(log.Topics[3][:]).BitLen() > 1 {
		return Spec{}, false, fmt.Errorf("%w: Velodrome creation shape", ErrInvalidLog)
	}
	t0, err := addressWord("token0", log.Topics[1][:])
	if err != nil {
		return Spec{}, false, err
	}
	t1, err := addressWord("token1", log.Topics[2][:])
	if err != nil {
		return Spec{}, false, err
	}
	address, err := addressWord("pool", log.Data[:32])
	if err != nil {
		return Spec{}, false, err
	}
	return Spec{Address: address, Token0: t0, Token1: t1, Kind: domain.PoolKindVelodromeV2, Factory: log.Address, Stable: log.Topics[3][31] == 1}, true, nil
}

// DecodeVelodromeV1PairCreated decodes Velodrome V1's
// PairCreated(address indexed token0, address indexed token1, bool stable,
// address pair, uint256): the stable flag and pair are data words.
func DecodeVelodromeV1PairCreated(log rpc.Log) (Spec, bool, error) {
	if !hasTopic0(log.Topics, topicVelodromeV1PairCreated) {
		return Spec{}, false, nil
	}
	if len(log.Topics) != 3 || len(log.Data) != 96 || new(big.Int).SetBytes(log.Data[:32]).BitLen() > 1 {
		return Spec{}, false, fmt.Errorf("%w: Velodrome V1 creation shape", ErrInvalidLog)
	}
	t0, err := addressWord("token0", log.Topics[1][:])
	if err != nil {
		return Spec{}, false, err
	}
	t1, err := addressWord("token1", log.Topics[2][:])
	if err != nil {
		return Spec{}, false, err
	}
	address, err := addressWord("pair", log.Data[32:64])
	if err != nil {
		return Spec{}, false, err
	}
	return Spec{Address: address, Token0: t0, Token1: t1, Kind: domain.PoolKindVelodromeV2, Factory: log.Address, Stable: log.Data[31] == 1}, true, nil
}

func (d *Discoverer) readVelodromeFees(ctx context.Context, candidates []*candidate, block uint64, stats *Stats) ([]*candidate, error) {
	var targets []*candidate
	var calls []pool.ContractCall
	for _, c := range candidates {
		if c.spec.Kind != domain.PoolKindVelodromeV2 {
			continue
		}
		request, err := velodromev2.FeeCall(pool.State{Key: c.spec.PoolKey(), Velodrome: &pool.VelodromeState{Factory: c.spec.Factory, Stable: c.spec.Stable}}, fmt.Sprintf("0x%x", block))
		if err != nil {
			return nil, err
		}
		targets = append(targets, c)
		calls = append(calls, request)
	}
	outputs, failures, err := d.callRound(ctx, calls)
	if err != nil {
		return nil, err
	}
	drop := map[*candidate]struct{}{}
	for i, c := range targets {
		if failures[i] != nil {
			if !isCallError(failures[i]) {
				return nil, failures[i]
			}
			stats.SkippedInvalid++
			drop[c] = struct{}{}
			continue
		}
		word, err := resultWord("Velodrome getFee", outputs[i])
		fee := new(big.Int).SetBytes(word)
		if err != nil || !fee.IsUint64() || fee.Uint64() > 100 || fee.Uint64()*100 > uint64(d.Config.MaxFee) {
			stats.SkippedFee++
			drop[c] = struct{}{}
			continue
		}
		c.spec.Fee = uint32(fee.Uint64()) * 100
	}
	return without(candidates, drop), nil
}
