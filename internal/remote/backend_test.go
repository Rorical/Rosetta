package remote

import (
	"io"
	"testing"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
)

func TestFactorySelectsL2InputsAndRejectsUnavailableCapabilities(t *testing.T) {
	for _, network := range []string{"arbitrum-one", "base", "optimism", "unichain", "ink"} {
		t.Run(network, func(t *testing.T) {
			cfg, err := config.Parse([]string{"-network", network, "-pool-config", "pools.json"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			backend, err := (Factory{}).Create(t.Context(), cfg)
			if err != nil || backend.Identity().ChainID != cfg.ChainID() {
				t.Fatal("dormant RPC backend unavailable", err)
			}
			if network != "arbitrum-one" {
				cfg.ExecutionConfig, cfg.SearchConfig = "execution.json", "search.json"
				if _, err := (Factory{}).Create(t.Context(), cfg); err != nil {
					t.Fatal("OP canonical confirmation not selectable", err)
				}
			}
			if network != "arbitrum-one" && backend.Identity().Family != domain.FamilyOPStack {
				t.Fatal("OP profile selected Nitro semantics")
			}
		})
	}
	// An explicit RPC source stays RPC-only on a preset that carries a
	// Subblocks stream: the profile default must not be applied.
	cfg, err := config.Parse([]string{"-network", "unichain", "-event-source", "rpc", "-pool-config", "pools.json"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FlashblocksURL != "" {
		t.Fatalf("rpc source kept a stream URL: %q", cfg.FlashblocksURL)
	}
	if _, err := (Factory{}).Create(t.Context(), cfg); err != nil {
		t.Fatal("RPC-only Unichain run rejected", err)
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{"missing catalog", nil},
		{"negative RPC pacing", []string{"-pool-config", "pools.json", "-rpc-min-interval", "-1s"}},
		{"conflicting sources", []string{"-pool-config", "pools.json", "-pool-snapshot", "snapshot.json"}},
		{"OP confirmation requires search", []string{"-network", "base", "-pool-config", "pools.json", "-execution-config", "execution.json"}},
		{"flash ignored source", []string{"-network", "base", "-pool-config", "pools.json", "-flashblocks", "wss://example.invalid", "-event-source", "rpc"}},
		{"profile stream with rpc source", []string{"-network", "unichain", "-pool-config", "pools.json", "-event-source", "rpc", "-flashblocks", "wss://example.invalid"}},
		{"unmeasured fast mapping", []string{"-pool-config", "pools.json", "-fast-feed", "wss://example.invalid"}},
		{"negative poll", []string{"-pool-config", "pools.json", "-rpc-poll", "-1ms"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := config.Parse(test.args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (Factory{}).Create(t.Context(), cfg); err == nil {
				t.Fatal("unsupported configuration silently accepted")
			}
		})
	}
	// A Flashblocks feed is an OP Stack property; the network profile check
	// rejects it before the backend sees a Nitro configuration.
	if _, err := config.Parse([]string{"-pool-config", "pools.json", "-flashblocks", "wss://example.invalid"}, io.Discard); err == nil {
		t.Fatal("Nitro network accepted a Flashblocks feed")
	}
}
