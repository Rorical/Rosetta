package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Network binds one instance to a chain, its public endpoints and its DEX
// deployments.
type Network struct {
	Family        domain.ChainFamily `json:"family,omitempty"`
	Name          string             `json:"name"`
	ChainID       uint64             `json:"chain_id"`
	ParentChainID uint64             `json:"parent_chain_id"`
	RPCURL        string             `json:"rpc"`
	FeedURL       string             `json:"feed"`
	// FlashblocksURL is the network's raw OP Stack subblocks stream. Only OP
	// Stack profiles may set it.
	FlashblocksURL string `json:"flashblocks,omitempty"`
	// SubmitURL is the network's sequencer ingress for signed transactions.
	SubmitURL string `json:"submit,omitempty"`
	// Ordering is required for Nitro profiles: a chain owner chooses it, so
	// one chain's policy says nothing about another's. OP Stack defaults to
	// priority ordering.
	Ordering domain.Ordering `json:"ordering,omitempty"`
	// BlockTimeMS is the nominal block interval. Intervals written as
	// durations are converted to blocks with it; zero takes the family's
	// default (Blocks).
	BlockTimeMS uint64 `json:"block_time_ms,omitempty"`
	// FeedCompression offers permessage-deflate to the standard feed, for
	// relays that require it.
	FeedCompression bool `json:"feed_compression,omitempty"`
	// FeedHeadersOptional accepts a feed handshake without the server
	// version and chain ID headers, checking them only when present.
	// The Robinhood and Arbitrum One relays send neither; blocks still come
	// from the chain-checked RPC.
	FeedHeadersOptional bool               `json:"feed_headers_optional,omitempty"`
	DEX                 domain.Deployments `json:"dex"`
}

// Blocks is the number of blocks, at least one, that span d.
func (n Network) Blocks(d time.Duration) uint64 {
	interval := time.Duration(n.BlockTimeMS) * time.Millisecond
	if interval == 0 {
		interval = 250 * time.Millisecond // Nitro's default
		if n.Family == domain.FamilyOPStack {
			interval = 2 * time.Second
		}
	}
	return max(1, uint64((d+interval-1)/interval))
}

// ChainID returns the selected chain, retaining Arbitrum One as the default
// for programmatic configurations as well as the command line.
func (c Config) ChainID() uint64 {
	if c.Network.ChainID != 0 {
		return c.Network.ChainID
	}
	return domain.ArbitrumOneChainID
}

// ResolveNetwork applies endpoint defaults only after selecting the network.
// Explicit endpoint overrides still have to pass runtime chain checks.
func (c Config) ResolveNetwork() (Config, error) {
	if c.NetworkFile != "" && c.NetworkName != "" {
		return Config{}, fmt.Errorf("-network and -network-file cannot be combined")
	}
	// Resolved profiles are carried by value through startup. Reopening a store
	// must not reread a file that can change after the instance selected its chain.
	if c.Network.ChainID == 0 {
		var err error
		if c.NetworkFile != "" {
			c.Network, err = loadNetwork(c.NetworkFile)
		} else {
			c.Network, err = builtinNetwork(c.NetworkName)
		}
		if err != nil {
			return Config{}, err
		}
	}
	if c.Network.Family == "" {
		c.Network.Family = domain.FamilyNitro
	}
	if c.Network.Family == domain.FamilyOPStack && c.Network.Ordering == "" {
		c.Network.Ordering = domain.OrderingPriority
	}
	if err := c.Network.validate(); err != nil {
		return Config{}, err
	}
	if c.RPCURL == "" {
		c.RPCURL = c.Network.RPCURL
	}
	if c.FeedURL == "" {
		c.FeedURL = c.Network.FeedURL
	}
	// An explicit -event-source rpc asks for RPC-only input, so a preset's
	// stream stays unused. "auto" is only the default and still adopts it.
	if c.FlashblocksURL == "" && c.EventSource != "rpc" {
		c.FlashblocksURL = c.Network.FlashblocksURL
	}
	if c.SubmitURL == "" {
		c.SubmitURL = c.Network.SubmitURL
	}
	if err := validateNetworkEndpoints(c.Network.Family, c.FlashblocksURL, c.SubmitURL); err != nil {
		return Config{}, err
	}
	return c, nil
}

func loadNetwork(path string) (Network, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Network{}, fmt.Errorf("read network profile: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var selected Network
	if err := decoder.Decode(&selected); err != nil {
		return Network{}, fmt.Errorf("decode network profile: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return Network{}, fmt.Errorf("network profile contains trailing JSON")
	}
	return selected, nil
}

func (n Network) validate() error {
	if n.Family != domain.FamilyNitro && n.Family != domain.FamilyOPStack {
		return fmt.Errorf("unsupported L2 chain family %q", n.Family)
	}
	if n.Name == "" || n.ChainID == 0 {
		return fmt.Errorf("network requires name and nonzero chain ID")
	}
	if err := validateNetworkEndpoints(n.Family, n.FlashblocksURL, n.SubmitURL); err != nil {
		return err
	}
	if n.Ordering != domain.OrderingPriority && n.Ordering != domain.OrderingArrival {
		return fmt.Errorf("network %s requires ordering %q or %q", n.Name, domain.OrderingPriority, domain.OrderingArrival)
	}
	if n.ParentChainID == 0 || n.ChainID == n.ParentChainID {
		return fmt.Errorf("rollup network requires a distinct nonzero parent chain ID")
	}
	if (n.DEX.V4PoolManager == (domain.Address{})) != (n.DEX.V4StateView == (domain.Address{})) {
		return fmt.Errorf("V4 pool manager and StateView must be configured together")
	}
	for source, address := range n.DEX.Factories {
		switch source {
		case "uniswap_v2", "uniswap_v3", "pancake_v3", "slipstream", "camelot_v3", "velodrome_v2", "velodrome_v1", "solidly_v3":
		default:
			return fmt.Errorf("unknown DEX factory %q", source)
		}
		if address == (domain.Address{}) {
			return fmt.Errorf("DEX factory %s is zero", source)
		}
	}
	return nil
}

func validateNetworkEndpoints(family domain.ChainFamily, flashblocks, submit string) error {
	if flashblocks != "" {
		if family != domain.FamilyOPStack {
			return fmt.Errorf("flashblocks feed requires an OP Stack network")
		}
		if err := validateEndpointURL("flashblocks", flashblocks, "ws", "wss"); err != nil {
			return err
		}
	}
	return validateEndpointURL("submit", submit, "http", "https")
}

func validateEndpointURL(name, value string, schemes ...string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("invalid %s URL %q", name, value)
	}
	for _, scheme := range schemes {
		if parsed.Scheme == scheme {
			return nil
		}
	}
	return fmt.Errorf("%s URL %q requires scheme %s", name, value, strings.Join(schemes, " or "))
}

func builtinNetwork(name string) (Network, error) {
	if network, ok := builtinOPNetwork(name); ok {
		return network, nil
	}
	switch name {
	case "", "arbitrum-one":
		// Ordered by priority fee. The public feed relay may omit its version
		// and chain ID headers, so they are optional.
		return Network{Name: "arbitrum-one", ChainID: 42161, ParentChainID: 1, RPCURL: "https://arb1.arbitrum.io/rpc", FeedURL: "wss://arb1-feed.arbitrum.io/feed", Ordering: domain.OrderingPriority, BlockTimeMS: 250, FeedHeadersOptional: true,
			DEX: domain.Deployments{Factories: map[string]domain.Address{
				"uniswap_v3": networkAddress("0x1f98431c8ad98523631ae4a59f267346ea31f984"), "pancake_v3": networkAddress("0x0bfbcf9fa4f9c56b0f40a671ad40e0805a091865"),
				"slipstream": networkAddress("0x88a60ee77392577c355c0acb2a8d476d5a4ec03a"), "uniswap_v2": networkAddress("0xf1D7CC64Fb4452F05c498126312eBE29f30Fbcf9"), "camelot_v3": networkAddress("0x1a3c9B1d2F0529D97f2afC5136Cc23e58f1FD35B"),
				// Solidly V3: rivals closed 102 loops through its 0x52ec30ce… pool in 79 h.
				"solidly_v3": networkAddress("0x70Fe4a44EA505cFa3A57b95cF2862D4fd5F0f687")},
				V4PoolManager: networkAddress("0x360e68faccca8ca495c1b759fd9eee466db9fb32"), V4StateView: networkAddress("0x76fd297e2d437cd7f76d50f01afe6160f86e9990")}}, nil
	case "robinhood":
		// First come, first served; blocks are about 100 ms apart.
		return Network{Name: name, ChainID: 4663, ParentChainID: 1, RPCURL: "https://rpc.mainnet.chain.robinhood.com", FeedURL: "wss://feed.mainnet.chain.robinhood.com", Ordering: domain.OrderingArrival, BlockTimeMS: 100, FeedCompression: true, FeedHeadersOptional: true,
			DEX: domain.Deployments{
				Factories: map[string]domain.Address{"uniswap_v2": networkAddress("0x8bceaa40b9acdfaedf85adf4ff01f5ad6517937f"), "uniswap_v3": networkAddress("0x1f7d7550b1b028f7571e69a784071f0205fd2efa"),
					// Aero Slipstream; its factory code differs from the OP build, so no local fee model.
					"slipstream": networkAddress("0xc63d7338c178e805c544b8f897066bd3bbcbe136")},
				V4PoolManager: networkAddress("0x8366a39cc670b4001a1121b8f6a443a643e40951"), V4StateView: networkAddress("0xf3334192d15450cdd385c8b70e03f9a6bd9e673b")}}, nil
	case "robinhood-testnet":
		// Assumed to order like mainnet; UNVERIFIED.
		return Network{Name: name, ChainID: 46630, ParentChainID: 11155111, RPCURL: "https://rpc.testnet.chain.robinhood.com", FeedURL: "wss://feed.testnet.chain.robinhood.com", Ordering: domain.OrderingArrival, FeedCompression: true, FeedHeadersOptional: true}, nil
	default:
		return Network{}, fmt.Errorf("unknown network %q; use a supported L2 preset or -network-file", name)
	}
}

func networkAddress(value string) domain.Address {
	address, _ := domain.ParseAddress(value)
	return address
}
