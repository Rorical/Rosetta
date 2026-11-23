package config

import "github.com/Rorical/Rosetta/internal/domain"

// OP profiles share execution semantics while deployment identities stay chain specific.
func builtinOPNetwork(name string) (Network, bool) {
	network := Network{Family: domain.FamilyOPStack, Name: name, ParentChainID: 1, Ordering: domain.OrderingPriority}
	switch name {
	case "base":
		network.ChainID, network.RPCURL, network.BlockTimeMS = 8453, "https://mainnet.base.org", 2000
		network.DEX = domain.Deployments{Factories: map[string]domain.Address{
			"uniswap_v2": networkAddress("0x8909Dc15e40173Ff4699343b6eB8132c65e18eC6"),
			"uniswap_v3": networkAddress("0x33128a8fC17869897dcE68Ed026d694621f6FDfD"),
			"slipstream": networkAddress("0xf8f2eB4940CFE7d13603DDDD87f123820Fc061Ef")},
			V4PoolManager: networkAddress("0x498581ff718922c3f8e6a244956af099b2652b2b"), V4StateView: networkAddress("0xa3c0c9b65bad0b08107aa264b0f3db444b867a71")}
	case "optimism":
		network.ChainID, network.RPCURL, network.BlockTimeMS = 10, "https://mainnet.optimism.io", 2000
		network.DEX = domain.Deployments{Factories: map[string]domain.Address{
			"uniswap_v2":   networkAddress("0x0c3c1c532F1e39EdF36BE9Fe0bE1410313E074Bf"),
			"slipstream":   networkAddress("0xfeab42d90055db3a0616249f76b6002d72266778"),
			"velodrome_v2": networkAddress("0xf1046053aa5682b4f9a81b5481394da16be5ff5a"),
			"velodrome_v1": networkAddress("0x25CbdDb98b35ab1FF77413456B31EC81A6B6B746"),
			"uniswap_v3":   networkAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
			"solidly_v3":   networkAddress("0x70Fe4a44EA505cFa3A57b95cF2862D4fd5F0f687")},
			V4PoolManager: networkAddress("0x9a13f98cb987694c9f086b1f5eb990eea8264ec3"), V4StateView: networkAddress("0xc18a3169788f4f75a170290584eca6395c75ecdb")}
	case "unichain":
		network.ChainID, network.RPCURL, network.BlockTimeMS = 130, "https://mainnet.unichain.org", 1000
		network.FlashblocksURL, network.SubmitURL = "wss://mainnet-flashblocks.unichain.org/ws", "https://mainnet-sequencer.unichain.org"
		network.DEX = domain.Deployments{Factories: map[string]domain.Address{
			"uniswap_v2": networkAddress("0x1f98400000000000000000000000000000000002"),
			"uniswap_v3": networkAddress("0x1f98400000000000000000000000000000000003")},
			V4PoolManager: networkAddress("0x1f98400000000000000000000000000000000004"), V4StateView: networkAddress("0x86e8631a016f9068c3f085faf484ee3f5fdee8f2")}
	case "ink":
		network.ChainID, network.RPCURL, network.BlockTimeMS = 57073, "https://rpc-gel.inkonchain.com", 1000
		network.DEX = domain.Deployments{V4PoolManager: networkAddress("0x360e68faccca8ca495c1b759fd9eee466db9fb32"), V4StateView: networkAddress("0x76fd297e2d437cd7f76d50f01afe6160f86e9990")}
	default:
		return Network{}, false
	}
	return network, true
}
