package config

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestNetworkSelectionAndEndpointDefaults(t *testing.T) {
	for _, test := range []struct {
		name          string
		child, parent uint64
	}{{"arbitrum-one", 42161, 1}, {"robinhood", 4663, 1}, {"robinhood-testnet", 46630, 11155111}} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := Parse([]string{"-network", test.name}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ChainID() != test.child || cfg.Network.ParentChainID != test.parent || cfg.FeedURL != cfg.Network.FeedURL || cfg.RPCURL != cfg.Network.RPCURL {
				t.Fatalf("wrong network defaults: %+v", cfg)
			}
			override, err := Parse([]string{"-network", test.name, "-rpc", "http://localhost:8547"}, io.Discard)
			if err != nil || override.RPCURL != "http://localhost:8547" || override.FeedURL != cfg.FeedURL {
				t.Fatal("explicit endpoint lost", err)
			}
		})
	}
	if _, err := Parse([]string{"-network", "unknown"}, io.Discard); err == nil {
		t.Fatal("accepted unknown network")
	}
}

func TestCustomNetworkStrictJSONAndDeploymentIsolation(t *testing.T) {
	first, err := Parse([]string{"-network", "robinhood"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	profile := first.Network
	profile.Name, profile.ChainID = "custom", 987654
	profile.DEX.Factories["uniswap_v3"] = domain.Address{9}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "network.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]string{"-network-file", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChainID() != 987654 || cfg.Network.DEX.Factories["uniswap_v3"] != (domain.Address{9}) {
		t.Fatal("custom profile did not roundtrip")
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.ResolveNetwork()
	if err != nil || resolved.ChainID() != 987654 {
		t.Fatal("resolved network changed after profile file modification", err)
	}
	fresh, _ := Parse([]string{"-network", "robinhood"}, io.Discard)
	if fresh.Network.DEX.Factories["uniswap_v3"] == (domain.Address{9}) {
		t.Fatal("instances share deployment map")
	}
	if _, err := Parse([]string{"-network", "robinhood", "-network-file", path}, io.Discard); err == nil {
		t.Fatal("ambiguous selection accepted")
	}
	for _, data := range []string{`{"chain_id":987654,"typo":true}`, `{} {}`, `{"dex":{"factories":{"uniswap_v3":"bad"}}}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Parse([]string{"-network-file", path}, io.Discard); err == nil {
			t.Fatal("invalid profile accepted", data)
		}
	}
}

func TestNetworkRequiresL2FamilyAndDistinctParent(t *testing.T) {
	baseline, err := (Config{}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name          string
		family        domain.ChainFamily
		chain, parent uint64
		valid         bool
	}{
		{"Ethereum L1", "ethereum", 1, 0, false},
		{"Nitro missing parent", domain.FamilyNitro, 42161, 0, false},
		{"OP missing parent", domain.FamilyOPStack, 10, 0, false},
		{"same chain and parent", domain.FamilyOPStack, 10, 10, false},
		{"Nitro L2", domain.FamilyNitro, 42161, 1, true},
		{"OP L2", domain.FamilyOPStack, 10, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := baseline
			cfg.Network.Family, cfg.Network.ChainID, cfg.Network.ParentChainID = test.family, test.chain, test.parent
			_, err := cfg.ResolveNetwork()
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}

func TestOptimismActiveFactoriesAreDefaultAndChainBound(t *testing.T) {
	op, err := builtinNetwork("optimism")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"slipstream": "0xfeab42d90055db3a0616249f76b6002d72266778", "velodrome_v2": "0xf1046053aa5682b4f9a81b5481394da16be5ff5a"} {
		want, err := domain.ParseAddress(text)
		if err != nil {
			t.Fatal(err)
		}
		if op.DEX.Factories[name] != want {
			t.Fatalf("missing active %s factory", name)
		}
	}
	arb, err := builtinNetwork("arbitrum-one")
	if err != nil {
		t.Fatal(err)
	}
	if arb.DEX.Factories["velodrome_v2"] != (domain.Address{}) || arb.DEX.Factories["slipstream"] == op.DEX.Factories["slipstream"] {
		t.Fatal("OP deployment leaked to Arbitrum")
	}
	if err = op.validate(); err != nil {
		t.Fatal(err)
	}
}

// Ordering decides whether a tip buys position, and each Nitro chain owner
// picks it: Arbitrum One runs a priority gas auction, Robinhood first come,
// first served. A Nitro profile that does not say is refused.
func TestNetworkOrderingIsStatedPerChain(t *testing.T) {
	for name, want := range map[string]domain.Ordering{"arbitrum-one": domain.OrderingPriority, "robinhood": domain.OrderingArrival, "optimism": domain.OrderingPriority} {
		cfg, err := Parse([]string{"-network", name}, io.Discard)
		if err != nil || cfg.Network.Ordering != want {
			t.Fatalf("%s: ordering %q, want %q (%v)", name, cfg.Network.Ordering, want, err)
		}
	}
	profile, err := builtinNetwork("robinhood")
	if err != nil {
		t.Fatal(err)
	}
	profile.Name, profile.ChainID, profile.Ordering = "orbit", 987655, ""
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "network.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]string{"-network-file", path}, io.Discard); err == nil {
		t.Fatal("accepted a Nitro profile without an ordering policy")
	}
}

// Robinhood's relay requires a compressed handshake. Neither relay sends the
// identity headers; Arbitrum One's handshake stays uncompressed.
func TestFeedHandshakeQuirks(t *testing.T) {
	for name, compressed := range map[string]bool{"robinhood": true, "robinhood-testnet": true, "arbitrum-one": false} {
		n, err := builtinNetwork(name)
		if err != nil {
			t.Fatal(err)
		}
		if n.FeedCompression != compressed || !n.FeedHeadersOptional {
			t.Fatalf("%s: compression %v, optional headers %v", name, n.FeedCompression, n.FeedHeadersOptional)
		}
	}
}

// The Unichain preset carries the public subblocks stream and sequencer
// ingress, so selecting the network also selects both endpoints.
func TestUnichainPresetCarriesSubblocksAndSequencer(t *testing.T) {
	cfg, err := Parse([]string{"-network", "unichain"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChainID() != 130 || cfg.FlashblocksURL != "wss://mainnet-flashblocks.unichain.org/ws" || cfg.SubmitURL != "https://mainnet-sequencer.unichain.org" {
		t.Fatalf("unichain endpoints not defaulted: chain %d flashblocks %q submit %q", cfg.ChainID(), cfg.FlashblocksURL, cfg.SubmitURL)
	}
	override, err := Parse([]string{"-network", "unichain", "-flashblocks", "ws://127.0.0.1:9001/ws", "-submit-rpc", "http://127.0.0.1:8545"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if override.FlashblocksURL != "ws://127.0.0.1:9001/ws" || override.SubmitURL != "http://127.0.0.1:8545" {
		t.Fatalf("explicit endpoints did not override the profile: %q %q", override.FlashblocksURL, override.SubmitURL)
	}
	v3, err := domain.ParseAddress("0x1f98400000000000000000000000000000000003")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.DEX.Factories["uniswap_v3"] != v3 {
		t.Fatal("unichain v3 factory missing")
	}
	if cfg.Network.DEX.V4PoolManager == (domain.Address{}) || cfg.Network.DEX.V4StateView == (domain.Address{}) {
		t.Fatal("unichain v4 deployment missing")
	}
}

// An explicit RPC-only event source leaves the preset's stream unused; the
// sequencer ingress still defaults because submitting does not read events.
func TestExplicitRPCSourceSkipsProfileStream(t *testing.T) {
	cfg, err := Parse([]string{"-network", "unichain", "-event-source", "rpc"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FlashblocksURL != "" || cfg.SubmitURL != "https://mainnet-sequencer.unichain.org" {
		t.Fatalf("rpc source kept the profile stream: flashblocks %q submit %q", cfg.FlashblocksURL, cfg.SubmitURL)
	}
	explicit, err := Parse([]string{"-network", "unichain", "-event-source", "rpc", "-flashblocks", "wss://127.0.0.1:9001/ws"}, io.Discard)
	if err != nil || explicit.FlashblocksURL != "wss://127.0.0.1:9001/ws" {
		t.Fatalf("explicit stream lost with rpc source: %q (%v)", explicit.FlashblocksURL, err)
	}
}

func TestNetworkProfileSubblocksAndSubmitEndpoints(t *testing.T) {
	profile, err := builtinNetwork("unichain")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "network.json")
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]string{"-network-file", path}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FlashblocksURL != profile.FlashblocksURL || cfg.SubmitURL != profile.SubmitURL {
		t.Fatalf("profile endpoints did not round-trip: %q %q", cfg.FlashblocksURL, cfg.SubmitURL)
	}

	nitro, err := builtinNetwork("arbitrum-one")
	if err != nil {
		t.Fatal(err)
	}
	nitro.Name, nitro.ChainID = "nitro-with-flashblocks", 987656
	nitro.FlashblocksURL = "wss://example.invalid/ws"
	data, err = json.Marshal(nitro)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]string{"-network-file", path}, io.Discard); err == nil {
		t.Fatal("Nitro profile accepted a Flashblocks feed")
	}
}

func TestNetworkEndpointSchemes(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"flashblocks over HTTPS", []string{"-network", "unichain", "-flashblocks", "https://example.invalid/ws"}},
		{"submit over WebSocket", []string{"-network", "unichain", "-submit-rpc", "wss://example.invalid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(test.args, io.Discard); err == nil {
				t.Fatal("wrong endpoint scheme accepted")
			}
		})
	}
}

func TestArbitrumNetworkKeepsNoOPEndpoints(t *testing.T) {
	cfg, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChainID() != 42161 || cfg.FlashblocksURL != "" || cfg.SubmitURL != "" {
		t.Fatalf("arbitrum gained OP endpoints: chain %d flashblocks %q submit %q", cfg.ChainID(), cfg.FlashblocksURL, cfg.SubmitURL)
	}
}
