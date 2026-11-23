package remote

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestCanonicalSlipstreamOracleDoesNotRepeatRPC(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	cfg, err := (config.Config{NetworkName: "optimism"}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	p := pool.State{Key: domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindSlipstream}, Slipstream: &pool.SlipstreamState{Valid: true}}
	spec := bootstrap.Spec{Address: p.Key.Address, Kind: p.Key.Kind}
	_, _, err = canonicalFees(t.Context(), cfg, rpc.NewClient(server.URL, rpc.Options{}), rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 11, Hash: domain.Hash{11}, Timestamp: 100}}, []bootstrap.Spec{spec}, []pool.State{p})
	if err != nil || calls.Load() != 0 {
		t.Fatalf("followed local model repeated canonical reads: %d %v", calls.Load(), err)
	}
	// A recognized factory with an unsupported hook is quarantined until a
	// relevant configuration event; retrying it each block wastes paid reads.
	p.Slipstream.Valid = false
	p.Slipstream.UnavailableReason = "unsupported_hook_code"
	_, _, err = canonicalFees(t.Context(), cfg, rpc.NewClient(server.URL, rpc.Options{}), rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 12, Hash: domain.Hash{12}, Timestamp: 102}}, []bootstrap.Spec{spec}, []pool.State{p})
	if err != nil || calls.Load() != 0 {
		t.Fatalf("quarantined model repeated reads: %d %v", calls.Load(), err)
	}
}
