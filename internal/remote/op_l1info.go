package remote

import (
	"bytes"
	"encoding/hex"
	"math/big"

	"github.com/ethereum/go-ethereum/rlp"
)

// opDepositType is the OP Stack deposit transaction type byte.
const opDepositType = 0x7e

// opFeeParams identifies an OP block's fee parameters. key changes with every
// fee field except the L1 base fee and blob base fee, which move with each L1
// block; l1Rate carries those two when known. Since Fjord the oracle's L1 fee
// is a size estimate times
//
//	16·baseFeeScalar·basefee + blobBaseFeeScalar·blobBaseFee
//
// over 1e12, so a fee read at one rate scales to another without a read, and
// the operator fee depends on neither. key "" means the parameters are unknown.
type opFeeParams struct {
	key    string
	l1Rate *big.Int
}

// l1InfoFeeParams reads the fee parameters an OP block's L1 attributes
// transaction sets in the L1Block predeploy. That transaction is the first of
// every block; it is packed calldata (Ecotone and later):
//
//	[0:4] selector, [4:8] baseFeeScalar, [8:12] blobBaseFeeScalar,
//	[12:20] sequenceNumber, [20:28] timestamp, [28:36] number,
//	[36:68] basefee, [68:100] blobBaseFee, [100:132] hash, [132:164] batcherHash,
//	[164:] operator fee scalar and constant (Isthmus), DA footprint scalar (Jovian).
//
// Its key format differs from rpc.BlockFeeParams: a switch between sources
// costs one extra fee read. It returns the zero value when raw is not such a
// transaction.
func l1InfoFeeParams(raw []byte) opFeeParams {
	if len(raw) < 2 || raw[0] != opDepositType {
		return opFeeParams{}
	}
	// sourceHash, from, to, mint, value, gas, isSystemTx, data
	var fields []rlp.RawValue
	if err := rlp.DecodeBytes(raw[1:], &fields); err != nil || len(fields) != 8 {
		return opFeeParams{}
	}
	var to, data []byte
	if rlp.DecodeBytes(fields[2], &to) != nil || !bytes.Equal(to, opL1Block[:]) || rlp.DecodeBytes(fields[7], &data) != nil || len(data) < 164 {
		return opFeeParams{}
	}
	key := append(bytes.Clone(data[:12]), data[164:]...)
	baseScalar := new(big.Int).SetBytes(data[4:8])
	blobScalar := new(big.Int).SetBytes(data[8:12])
	rate := new(big.Int).Mul(baseScalar, new(big.Int).SetBytes(data[36:68]))
	rate.Mul(rate, big.NewInt(16))
	rate.Add(rate, blobScalar.Mul(blobScalar, new(big.Int).SetBytes(data[68:100])))
	return opFeeParams{key: "l1info:" + hex.EncodeToString(key), l1Rate: rate}
}
