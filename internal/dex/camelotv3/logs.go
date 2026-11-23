package camelotv3

import (
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
	"golang.org/x/crypto/sha3"
)

var (
	TopicFee         = metadataTopic("Fee(uint16,uint16)")
	TopicTickSpacing = metadataTopic("TickSpacing(int24)")
)

type MetadataEvent struct {
	Fee, FeeOneForZero uint32
	// Positive only for a TickSpacing event; zero selects the Fee event.
	TickSpacing int32
}

// DecodeMetadata reads Camelot V3's mutable fee/spacing events. Swap/Mint/Burn
// retain the V3 event ABI and are decoded by the ordinary V3 reducer.
func DecodeMetadata(topics []domain.Hash, data []byte) (MetadataEvent, bool, error) {
	if len(topics) == 0 || (topics[0] != TopicFee && topics[0] != TopicTickSpacing) {
		return MetadataEvent{}, false, nil
	}
	words := 1
	if topics[0] == TopicFee {
		words = 2
	}
	if len(topics) != 1 || len(data) != words*abiWordSize {
		return MetadataEvent{}, false, fmt.Errorf("%w: metadata event shape", ErrInvalidLog)
	}
	if words == 1 {
		value, err := signedWord("tick spacing", data, 24)
		if err != nil || value.Sign() <= 0 {
			return MetadataEvent{}, false, fmt.Errorf("%w: nonpositive or invalid tick spacing", ErrInvalidLog)
		}
		return MetadataEvent{TickSpacing: int32(value.Int64())}, true, nil
	}
	forward, err := unsignedWord("forward fee", data[:abiWordSize], 16)
	if err != nil {
		return MetadataEvent{}, false, fmt.Errorf("%w: %v", ErrInvalidLog, err)
	}
	reverse, err := unsignedWord("reverse fee", data[abiWordSize:], 16)
	if err != nil {
		return MetadataEvent{}, false, fmt.Errorf("%w: %v", ErrInvalidLog, err)
	}
	return MetadataEvent{Fee: uint32(forward.Uint64()), FeeOneForZero: uint32(reverse.Uint64())}, true, nil
}

func metadataTopic(text string) domain.Hash {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(text))
	var topic domain.Hash
	copy(topic[:], h.Sum(nil))
	return topic
}
