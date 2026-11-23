package discovery

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
)

// Source is one log stream a Discoverer scans for new pools.
type Source uint8

const (
	SourceUniswapV3 Source = iota + 1
	SourcePancakeV3
	SourceSlipstream
	SourceUniswapV4
	SourceUniswapV2
	SourceCamelotV3
	SourceVelodromeV2
	SourceSolidlyV3
	SourceVelodromeV1
)

// String returns the configuration name of the source.
func (s Source) String() string {
	switch s {
	case SourceVelodromeV2:
		return "velodrome_v2"
	case SourceUniswapV3:
		return "uniswap_v3"
	case SourcePancakeV3:
		return "pancake_v3"
	case SourceSlipstream:
		return "slipstream"
	case SourceUniswapV4:
		return "uniswap_v4"
	case SourceUniswapV2:
		return "uniswap_v2"
	case SourceCamelotV3:
		return "camelot_v3"
	case SourceSolidlyV3:
		return "solidly_v3"
	case SourceVelodromeV1:
		return "velodrome_v1"
	default:
		return fmt.Sprintf("Source(%d)", uint8(s))
	}
}

// Valid reports whether the source is one this package scans.
func (s Source) Valid() bool {
	return s >= SourceUniswapV3 && s <= SourceVelodromeV1
}

// Contract is the verified creation emitter (factory, or V4 PoolManager).
func (s Source) Contract(deployments ...*domain.Deployments) domain.Address {
	if len(deployments) > 0 && deployments[0] != nil {
		d := deployments[0]
		if s == SourceUniswapV4 {
			return d.V4PoolManager
		}
		return d.Factories[s.String()]
	}
	return s.address()
}

// PoolKind identifies the adapter family produced by this source.
func (s Source) PoolKind() domain.PoolKind {
	switch s {
	case SourceVelodromeV2, SourceVelodromeV1:
		// V1 pairs share V2's invariants and executor interface.
		return domain.PoolKindVelodromeV2
	case SourceUniswapV2:
		return domain.PoolKindV2
	case SourceUniswapV3, SourcePancakeV3:
		return domain.PoolKindV3
	case SourceSlipstream:
		return domain.PoolKindSlipstream
	case SourceCamelotV3:
		return domain.PoolKindCamelotV3
	case SourceSolidlyV3:
		return domain.PoolKindSolidlyV3
	case SourceUniswapV4:
		return domain.PoolKindV4
	default:
		return 0
	}
}

// Matches identifies a configured creation-event stream before decoding its
// payload. Both the emitter and event signature must match.
func (s Source) Matches(address domain.Address, topics []domain.Hash, deployments ...*domain.Deployments) bool {
	return address != (domain.Address{}) && s.Valid() && address == s.Contract(deployments...) && len(topics) > 0 && topics[0] == s.topic0()
}

// ParseSource maps a configuration string to a Source. Names are matched
// case-insensitively after trimming surrounding space.
func ParseSource(text string) (Source, error) {
	switch normalizeName(text) {
	case "velodrome_v2":
		return SourceVelodromeV2, nil
	case "uniswap_v3":
		return SourceUniswapV3, nil
	case "pancake_v3":
		return SourcePancakeV3, nil
	case "slipstream":
		return SourceSlipstream, nil
	case "uniswap_v4":
		return SourceUniswapV4, nil
	case "uniswap_v2":
		return SourceUniswapV2, nil
	case "camelot_v3":
		return SourceCamelotV3, nil
	case "solidly_v3":
		return SourceSolidlyV3, nil
	case "velodrome_v1":
		return SourceVelodromeV1, nil
	default:
		return 0, fmt.Errorf("%w: unknown source %q", ErrInvalidConfig, text)
	}
}

// allSources is the default scan set and the order used when configuration
// leaves sources unset.
func allSources() []Source {
	return []Source{SourceUniswapV3, SourcePancakeV3, SourceSlipstream, SourceUniswapV4, SourceUniswapV2, SourceCamelotV3}
}

// address is the contract that emits the creation events for this source.
func (s Source) address() domain.Address {
	switch s {
	case SourceUniswapV3:
		return uniswapV3Factory
	case SourcePancakeV3:
		return pancakeV3Factory
	case SourceSlipstream:
		return slipstreamFactory
	case SourceUniswapV4:
		return v4PoolManager
	case SourceUniswapV2:
		return uniswapV2Factory
	case SourceCamelotV3:
		return camelotV3Factory
	default:
		return domain.Address{}
	}
}

// topic0 is the signature topic of the pool creation event.
func (s Source) topic0() domain.Hash {
	switch s {
	case SourceVelodromeV2:
		return topicVelodromePoolCreated
	case SourceVelodromeV1:
		return topicVelodromeV1PairCreated
	case SourceUniswapV3, SourcePancakeV3, SourceSolidlyV3:
		// Solidly V3 keeps the signature but indexes tickSpacing, not fee.
		return topicV3PoolCreated
	case SourceSlipstream:
		return topicSlipstreamPoolCreated
	case SourceUniswapV4:
		return topicV4Initialize
	case SourceUniswapV2:
		return topicV2PairCreated
	case SourceCamelotV3:
		return topicCamelotPool
	default:
		return domain.Hash{}
	}
}

// Arbitrum One defaults, verified against the chain. Runtime callers pass their
// selected deployments explicitly; event signatures are shared across chains.
var (
	uniswapV3Factory = domain.Address{
		0x1f, 0x98, 0x43, 0x1c, 0x8a, 0xd9, 0x85, 0x23,
		0x63, 0x1a, 0xe4, 0xa5, 0x9f, 0x26, 0x73, 0x46,
		0xea, 0x31, 0xf9, 0x84,
	}
	pancakeV3Factory = domain.Address{
		0x0b, 0xfb, 0xcf, 0x9f, 0xa4, 0xf9, 0xc5, 0x6b,
		0x0f, 0x40, 0xa6, 0x71, 0xad, 0x40, 0xe0, 0x80,
		0x5a, 0x09, 0x18, 0x65,
	}
	slipstreamFactory = domain.Address{
		0x88, 0xa6, 0x0e, 0xe7, 0x73, 0x92, 0x57, 0x7c,
		0x35, 0x5c, 0x0a, 0xcb, 0x2a, 0x8d, 0x47, 0x6d,
		0x5a, 0x4e, 0xc0, 0x3a,
	}
	// v4PoolManager is the singleton every V4 pool lives behind. The address
	// is shared with internal/dex/uniswapv4 so the two cannot drift.
	v4PoolManager = uniswapv4.PoolManager()
	// stateView is the parsed form of the exported StateView address. The
	// source is a package constant, so parseStaticAddress cannot fail in a
	// built binary; TestConstants pins the result.
	stateView          = parseStaticAddress(uniswapv4.StateViewAddress)
	topicV3PoolCreated = domain.Hash{
		0x78, 0x3c, 0xca, 0x1c, 0x04, 0x12, 0xdd, 0x0d,
		0x69, 0x5e, 0x78, 0x45, 0x68, 0xc9, 0x6d, 0xa2,
		0xe9, 0xc2, 0x2f, 0xf9, 0x89, 0x35, 0x7a, 0x2e,
		0x8b, 0x1d, 0x9b, 0x2b, 0x4e, 0x6b, 0x71, 0x18,
	}
	topicSlipstreamPoolCreated = domain.Hash{
		0xab, 0x0d, 0x57, 0xf0, 0xdf, 0x53, 0x7b, 0xb2,
		0x5e, 0x80, 0x24, 0x5e, 0xf7, 0x74, 0x8f, 0xa6,
		0x23, 0x53, 0x80, 0x8c, 0x54, 0xd6, 0xe5, 0x28,
		0xa9, 0xdd, 0x20, 0x88, 0x7a, 0xed, 0x9a, 0xc2,
	}
	topicV4Initialize = domain.Hash{
		0xdd, 0x46, 0x6e, 0x67, 0x4e, 0xa5, 0x57, 0xf5,
		0x62, 0x95, 0xe2, 0xd0, 0x21, 0x8a, 0x12, 0x5e,
		0xa4, 0xb4, 0xf0, 0xf6, 0xf3, 0x30, 0x7b, 0x95,
		0xf8, 0x5e, 0x61, 0x10, 0x83, 0x8d, 0x64, 0x38,
	}
)

// parseStaticAddress parses an address the package controls. A malformed
// constant is a build error caught by TestConstants; returning the zero
// address keeps library code panic-free.
func parseStaticAddress(text string) domain.Address {
	address, err := domain.ParseAddress(text)
	if err != nil {
		return domain.Address{}
	}
	return address
}
