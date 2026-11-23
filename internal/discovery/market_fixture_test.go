package discovery

import (
	"math/big"
	"math/rand/v2"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// syntheticMarket is a deterministic OP-shaped pool universe: four hubs
// (WETH, USDC, USDT, OP), a mid tier connected to several hubs, and long-tail
// tokens with one to three pools each. About two thirds of the pools are
// pinned as the existing catalog; the rest are discovery candidates.
type syntheticMarket struct {
	rng    *rand.Rand
	hubs   []domain.Address
	mids   []domain.Address
	pools  []pool.State
	pinned []domain.PoolKey
}

func newSyntheticMarket(seed uint64, tails int) *syntheticMarket {
	m := &syntheticMarket{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
	for range 4 {
		m.hubs = append(m.hubs, m.address())
	}
	for range 12 {
		m.mids = append(m.mids, m.address())
	}
	for i, a := range m.hubs {
		for _, b := range m.hubs[i+1:] {
			for range 3 {
				m.addPool(a, b)
			}
		}
	}
	for _, mid := range m.mids {
		for _, hub := range m.pick(m.hubs, 2+m.rng.IntN(2)) {
			for range 1 + m.rng.IntN(2) {
				m.addPool(mid, hub)
			}
		}
	}
	for range 15 {
		pair := m.pick(m.mids, 2)
		m.addPool(pair[0], pair[1])
	}
	for range tails {
		m.addTail()
	}
	for _, p := range m.pools {
		if m.rng.IntN(3) != 0 {
			m.pinned = append(m.pinned, p.Key)
		}
	}
	return m
}

func (m *syntheticMarket) address() domain.Address {
	var address domain.Address
	for i := range address {
		address[i] = byte(m.rng.Uint32())
	}
	return address
}

func (m *syntheticMarket) pick(from []domain.Address, n int) []domain.Address {
	order := m.rng.Perm(len(from))
	out := make([]domain.Address, 0, n)
	for _, i := range order[:n] {
		out = append(out, from[i])
	}
	return out
}

// addTail adds a long-tail token with a WETH-heavy first pool and occasional
// second and third pools to other hubs or mid-tier tokens.
func (m *syntheticMarket) addTail() domain.Address {
	token := m.address()
	first := m.hubs[0]
	if m.rng.IntN(2) == 0 {
		first = m.hubs[m.rng.IntN(len(m.hubs))]
	}
	m.addPool(token, first)
	connectors := append(append([]domain.Address{}, m.hubs...), m.mids...)
	if m.rng.IntN(5) < 2 {
		m.addPool(token, connectors[m.rng.IntN(len(connectors))])
	}
	if m.rng.IntN(10) == 0 {
		m.addPool(token, connectors[m.rng.IntN(len(connectors))])
	}
	return token
}

func (m *syntheticMarket) addPool(a, b domain.Address) pool.State {
	p := pool.State{
		Key:      domain.PoolKey{Address: m.address(), Kind: domain.PoolKindV2},
		Token0:   a,
		Token1:   b,
		Reserve0: new(big.Int).Lsh(big.NewInt(1), 90),
		Reserve1: new(big.Int).Lsh(big.NewInt(1), 90),
		Fee:      3000,
	}
	m.pools = append(m.pools, p)
	return p
}

func (m *syntheticMarket) policy() SelectionPolicy {
	return SelectionPolicy{
		Targets:        []SelectionTarget{{Token: m.hubs[0].Hex(), Amount: "1000000000000000000"}},
		MaxTokens:      256,
		MaxSlippageBPS: 100,
		ActivityWindow: 100,
		ProbeLimit:     128,
	}
}
