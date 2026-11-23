package discovery

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestVelodromeCreationCurveAndActivity(t *testing.T) {
	factory := parseStaticAddress("0xf1046053aa5682b4f9a81b5481394da16be5ff5a")
	pool := parseStaticAddress("0x4f3da11c5cadf644ae023dbad01008a934c993e2")
	t0 := parseStaticAddress("0x0b2c639c533813f4aa9d7837caf62653d097ff85")
	t1 := parseStaticAddress("0xc40f949f8a4e094d1b49a23ea9241d289b7b2819")
	deployments := &domain.Deployments{Factories: map[string]domain.Address{"velodrome_v2": factory}}
	config, e := LoadConfigForNetwork([]byte(`{}`), deployments)
	if e != nil {
		t.Fatal(e)
	}
	if len(config.Emitters()) != 1 || config.Emitters()[0].Source != SourceVelodromeV2 {
		t.Fatal(config.Emitters())
	}
	log := rpc.Log{Address: factory, Topics: []domain.Hash{topicVelodromePoolCreated, {}, {}, {}}, Data: make([]byte, 64)}
	copy(log.Topics[1][12:], t0[:])
	copy(log.Topics[2][12:], t1[:])
	log.Topics[3][31] = 1
	copy(log.Data[12:32], pool[:])
	big.NewInt(42).FillBytes(log.Data[32:])
	spec, ok, e := DecodeCreation(SourceVelodromeV2, log, deployments)
	if e != nil || !ok || spec.Kind != domain.PoolKindVelodromeV2 || !spec.Stable || spec.Factory != factory || spec.Token0 != t0 || !config.AcceptSpec(spec) {
		t.Fatal(spec, ok, e)
	}
	log.Topics[3][31] = 2
	if _, _, e = DecodeCreation(SourceVelodromeV2, log, deployments); e == nil {
		t.Fatal("non-boolean curve accepted")
	}
	book := &ActivityBook{}
	book.Observe(domain.BlockRef{Number: 10, Hash: domain.Hash{1}}, []rpc.Log{{Address: pool, Topics: []domain.Hash{swapVelodrome}}}, nil, 7200)
	if len(book.Unknown()) != 1 || ActivityFor(book.Snapshot(), spec.PoolKey()).Swaps != 1 {
		t.Fatal("Velodrome activity not discovered")
	}
}
