package velodromev2

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"math/big"
	"testing"
)

func TestStableRankingRespectsRecordedOutputInventory(t *testing.T) {
	p := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindVelodromeV2, Address: address("0x0c3a7aa27a6e14b7a92e09456b0970ab71cc0598")}, Token0: address("0x4200000000000000000000000000000000000042"), Token1: address("0x7f5c764cbc14f9669b88837ca1490cca17c31607"), Fee: 500, Reserve0: integer("1422469443274152029607"), Reserve1: integer("53627356"), Velodrome: &pool.VelodromeState{Factory: factory, Stable: true, Scale0: integer("1000000000000000000"), Scale1: integer("1000000")}}
	model, err := RankingModel(p)
	if err != nil {
		t.Fatal(err)
	}
	amount := integer("526859274660614361708")
	charged := new(big.Int).Mul(amount, big.NewInt(999500))
	estimated := new(big.Int).Quo(new(big.Int).Mul(charged, model.Reserve1), new(big.Int).Add(new(big.Int).Mul(model.Reserve0, big.NewInt(1000000)), charged))
	if estimated.Cmp(p.Reserve1) >= 0 {
		t.Fatalf("model promised %s units from %s inventory", estimated, p.Reserve1)
	}
	exact, err := New().QuoteExactIn(t.Context(), p, pool.DeepState{}, p.Token0, amount)
	if err != nil || exact.String() != "32753480" {
		t.Fatal("historical exact output changed", exact, err)
	}
	if model.ReverseOut.Cmp(p.Reserve0) != 0 || model.ConstantProduct {
		t.Fatal("reverse capacity or model proof classification differs")
	}
	// Scaling actual inventory and input together must scale the screening curve.
	doubled := p.Copy()
	doubled.Reserve0.Mul(doubled.Reserve0, big.NewInt(2))
	doubled.Reserve1.Mul(doubled.Reserve1, big.NewInt(2))
	bigger, err := RankingModel(doubled)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]*big.Int{{model.Reserve0, bigger.Reserve0}, {model.Reserve1, bigger.Reserve1}, {model.ReverseIn, bigger.ReverseIn}, {model.ReverseOut, bigger.ReverseOut}} {
		delta := new(big.Int).Sub(pair[1], new(big.Int).Mul(pair[0], big.NewInt(2)))
		if delta.Sign() < 0 || delta.Cmp(big.NewInt(1)) > 0 {
			t.Fatal("screening liquidity does not scale with inventory", delta)
		}
	}
}
