package discovery

import (
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// DecodeV2PairCreated reads the canonical Uniswap V2 PairCreated event.
// Its fixed swap fee is 3000 pips; Camelot V2 has different fee/stable math
// and is not admitted by this source.
func DecodeV2PairCreated(log rpc.Log) (Spec, bool, error) {
	return decodePairCreation(log, topicV2PairCreated, 2, domain.PoolKindV2, 3000)
}

// DecodeCamelotPool reads Pool(token0,token1,pool). Fee and mutable spacing
// are absent from the event and must be read from the selected block state.
func DecodeCamelotPool(log rpc.Log) (Spec, bool, error) {
	return decodePairCreation(log, topicCamelotPool, 1, domain.PoolKindCamelotV3, 0)
}

func decodePairCreation(log rpc.Log, topic domain.Hash, words int, kind domain.PoolKind, fee uint32) (Spec, bool, error) {
	if !hasTopic0(log.Topics, topic) {
		return Spec{}, false, nil
	}
	if len(log.Topics) != 3 || len(log.Data) != words*abiWordSize {
		return Spec{}, false, fmt.Errorf("%w: pair creation topics/data shape", ErrInvalidLog)
	}
	token0, err := addressWord("token0", log.Topics[1][:])
	if err != nil {
		return Spec{}, false, err
	}
	token1, err := addressWord("token1", log.Topics[2][:])
	if err != nil {
		return Spec{}, false, err
	}
	address, err := addressWord("pool", log.Data[:abiWordSize])
	if err != nil {
		return Spec{}, false, err
	}
	return Spec{Address: address, Token0: token0, Token1: token1, Kind: kind, Fee: fee}, true, nil
}
