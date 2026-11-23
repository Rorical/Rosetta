package ingest

import (
	"hash"
	"math/big"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
)

// fastReceiptDigest binds the receipt fields that identify execution. Transport
// creation time and auction round do not change transaction execution identity.
func fastReceiptDigest(message feed.FastMessage, effects domain.Hash) domain.Hash {
	h := sha3.NewLegacyKeccak256()
	h.Write(effects[:])
	writeFastUint(h, uint64(len(message.RawTx)))
	h.Write(message.RawTx)
	r := message.Receipt
	writeFastUint(h, r.GasUsed)
	writeFastUint(h, r.GasUsedForL1)
	writeFastUint(h, r.CumulativeGasUsed)
	writeFastBig(h, r.EffectiveGasPrice)
	writeFastBig(h, r.BaseFee)
	h.Write(r.ContractAddress[:])
	var result domain.Hash
	copy(result[:], h.Sum(nil))
	return result
}

func writeFastBig(h hash.Hash, value *big.Int) {
	if value == nil {
		h.Write([]byte{0})
		return
	}
	h.Write([]byte{byte(value.Sign() + 2)})
	data := value.Bytes()
	writeFastUint(h, uint64(len(data)))
	h.Write(data)
}
