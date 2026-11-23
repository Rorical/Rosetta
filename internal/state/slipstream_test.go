package state

import (
	"encoding/json"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"golang.org/x/crypto/sha3"
	"math/big"
	"testing"
)

func slipstreamPool() pool.State {
	p := v3State(testAddress(0x43))
	p.Key.Kind = domain.PoolKindSlipstream
	p.Tick = 20
	p.Slipstream = &pool.SlipstreamState{Valid: true, Factory: testAddress(9), Hook: testAddress(8), Block: 10, Timestamp: 100, BaseFee: 100, SpacingFee: 500, DefaultFeeCap: 50000, DefaultScalingFactor: 1_000_000, SecondsAgo: 10, ObservationIndex: 1, Cardinality: 2, CardinalityNext: 2, Complete: true, Observations: pool.NewOracleHistory([]pool.OracleObservation{{Index: 0, Timestamp: 90}, {Index: 1, Timestamp: 100, TickCumulative: 100}})}
	slipstream.RefreshFee(&p)
	return p
}

func TestSlipstreamTimeSwapRollbackAndPersistence(t *testing.T) {
	s := New()
	p := slipstreamPool()
	if err := s.BeginBlock(domain.BlockRef{Number: 10, Timestamp: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(p); err != nil {
		t.Fatal(err)
	}
	s.Commit()
	s.ClearDirty()
	before := s.View()
	if err := s.BeginBlock(domain.BlockRef{Number: 11, Timestamp: 102}); err != nil {
		t.Fatal(err)
	}
	if len(s.Dirty()) != 1 {
		t.Fatal("time-only fee update not dirty")
	}
	if err := s.ApplyV3(p.Key, uniswapv3.Event{Kind: uniswapv3.EventSwap, Tick: 40, SqrtPriceX96: big.NewInt(100), Liquidity: big.NewInt(100)}, 11); err != nil {
		t.Fatal(err)
	}
	updated, _ := s.Pool(p.Key)
	if updated.Fee != 108 || updated.Slipstream.BlockFee != 108 {
		t.Fatalf("pre-swap block cache %+v", updated)
	}
	raw, err := s.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var wire snapshotWire
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Version != OracleStateVersion {
		t.Fatal("old decoder would lose oracle")
	}
	restored := New()
	if err = restored.LoadSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	got, _ := restored.Pool(p.Key)
	if got.Slipstream.Observations.Len() != 2 || got.Slipstream.BlockFee != 108 {
		t.Fatal("oracle lost at restart")
	}
	if err = s.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Pool(p.Key)
	old, _ := before.Pool(p.Key)
	if got.Fee != old.Fee || got.Slipstream.ObservationIndex != old.Slipstream.ObservationIndex || got.Slipstream.BlockFee != 0 {
		t.Fatal("rollback leaked branch oracle")
	}
}

func TestSlipstreamConfigurationInvalidatesAndRollsBack(t *testing.T) {
	s := New()
	p := slipstreamPool()
	s.Upsert(p)
	s.ClearDirty()
	s.BeginBlock(domain.BlockRef{Number: 10, Timestamp: 100})
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte("SecondsAgoSet(uint32)"))
	var topic domain.Hash
	copy(topic[:], h.Sum(nil))
	s.ApplySlipstreamConfiguration(testAddress(7), []domain.Hash{topic})
	current, _ := s.Pool(p.Key)
	if !current.Slipstream.Valid {
		t.Fatal("unrelated emitter invalidates model")
	}
	s.ApplySlipstreamConfiguration(p.Slipstream.Hook, []domain.Hash{topic})
	current, _ = s.Pool(p.Key)
	if _, err := slipstream.LocalFee(current); err == nil {
		t.Fatal("changed config still quotable")
	}
	if len(s.Dirty()) != 1 {
		t.Fatal("invalidated model not dirty")
	}
	if err := s.Rollback(); err != nil {
		t.Fatal(err)
	}
	current, _ = s.Pool(p.Key)
	if !current.Slipstream.Valid {
		t.Fatal("rollback lost config")
	}
}
