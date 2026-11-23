package discovery

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"golang.org/x/crypto/sha3"
)

// Deployment identities from official Uniswap/Camelot documentation.
// Runtime chain coverage still requires local canonical state.
var (
	uniswapV2Factory          = parseStaticAddress("0xf1D7CC64Fb4452F05c498126312eBE29f30Fbcf9")
	camelotV3Factory          = parseStaticAddress("0x1a3c9B1d2F0529D97f2afC5136Cc23e58f1FD35B")
	topicV2PairCreated        = eventSignature("PairCreated(address,address,address,uint256)")
	topicCamelotPool          = eventSignature("Pool(address,address,address)")
	topicVelodromePoolCreated = eventSignature("PoolCreated(address,address,bool,address,uint256)")
	// Velodrome V1: the stable flag is data, not a topic.
	topicVelodromeV1PairCreated = eventSignature("PairCreated(address,address,bool,address,uint256)")
)

func eventSignature(signature string) domain.Hash {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(signature))
	var result domain.Hash
	copy(result[:], h.Sum(nil))
	return result
}
