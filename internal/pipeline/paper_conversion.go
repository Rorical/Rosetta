package pipeline

import (
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type conversionPath struct {
	keys     []domain.PoolKey
	tokens   []domain.Address
	capacity *big.Int // Bottleneck spot reserve, expressed in wrapped-native units.
	rate     *big.Rat // Destination units per wrapped-native unit.
}

// Build once per canonical observation, not per candidate. At most three hops;
// prefer fewer hops, then the largest bottleneck spot reserve. Sorted input
// provides deterministic tie breaking. Prices are re-read from the quote view.
func (b *paperBook) prepareConversions(pools []pool.State) {
	wrapped, _ := domain.ParseAddress(b.policy.WrappedNative)
	b.conversions = conversionPaths(pools, wrapped)
}

// conversionPaths finds, for every token within three hops of the wrapped
// native, the pools that price it.
func conversionPaths(pools []pool.State, wrapped domain.Address) map[domain.Address]conversionPath {
	paths := map[domain.Address]conversionPath{wrapped: {tokens: []domain.Address{wrapped}, rate: big.NewRat(1, 1)}}
	for range 3 {
		next := make(map[domain.Address]conversionPath, len(paths))
		for token, path := range paths {
			next[token] = path
		}
		for _, p := range pools {
			for _, token := range []domain.Address{p.Token0, p.Token1} {
				prefix, ok := paths[token]
				if !ok {
					continue
				}
				out := p.Token1
				if token == p.Token1 {
					out = p.Token0
				}
				if slices.Contains(prefix.tokens, out) {
					continue
				}
				existing, exists := next[out]
				if exists && len(existing.keys) < len(prefix.keys)+1 {
					continue
				}
				n, d, ok := paperSpot(p, token)
				if !ok {
					continue
				}
				r0, r1, ok := pool.SpotReserves(p)
				if p.Key.Kind == domain.PoolKindVelodromeV2 {
					r0, r1 = p.Reserve0, p.Reserve1
					ok = r0 != nil && r1 != nil && r0.Sign() > 0 && r1.Sign() > 0
				}
				if !ok {
					continue
				}
				reserve := r0
				if token == p.Token1 {
					reserve = r1
				}
				capacity := new(big.Int).Mul(reserve, prefix.rate.Denom())
				capacity.Quo(capacity, prefix.rate.Num())
				if prefix.capacity != nil && prefix.capacity.Cmp(capacity) < 0 {
					capacity.Set(prefix.capacity)
				}
				rate := new(big.Rat).Mul(prefix.rate, new(big.Rat).SetFrac(n, d))
				if capacity.Sign() == 0 || exists && len(existing.keys) == len(prefix.keys)+1 && existing.capacity.Cmp(capacity) >= 0 {
					continue
				}
				next[out] = conversionPath{append(slices.Clone(prefix.keys), p.Key), append(slices.Clone(prefix.tokens), out), capacity, rate}
			}
		}
		paths = next
	}
	return paths
}

func (b *paperBook) convertOutsideRoute(view arb.View, asset, wrapped domain.Address, native *big.Int) (*big.Int, []domain.PoolKey, bool) {
	path, ok := b.conversions[asset]
	if !ok || len(path.keys) == 0 {
		return nil, nil, false
	}
	token := wrapped
	converted := new(big.Int).Set(native)
	for _, key := range path.keys {
		p, ok := view.Pool(key)
		if !ok {
			return nil, nil, false
		}
		n, d, ok := paperSpot(p, token)
		if !ok {
			return nil, nil, false
		}
		product := new(big.Int).Mul(converted, n)
		remainder := new(big.Int)
		converted.QuoRem(product, d, remainder)
		if remainder.Sign() > 0 {
			converted.Add(converted, big.NewInt(1))
		}
		if token == p.Token0 {
			token = p.Token1
		} else {
			token = p.Token0
		}
	}
	if token != asset {
		return nil, nil, false
	}
	return converted, slices.Clone(path.keys), true
}
