package ingest

import (
	"encoding/binary"
	"hash"
	"math/big"
	"slices"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
)

func fastTransaction(message feed.FastMessage) Transaction {
	tx := Transaction{Hash: message.TxHash, Index: message.TxIndex, Successful: message.Receipt.Successful, Logs: make([]Log, len(message.Receipt.Logs)), Raw: message.RawTx}
	for i, log := range message.Receipt.Logs {
		tx.Logs[i] = Log{Address: log.Address, Topics: log.Topics, Data: log.Data}
	}
	return tx
}

// The digest binds ordered effects, including reverted/no-op receipts. Length
// prefixes prevent different topic/data partitions from sharing an encoding.
func transactionDigest(tx Transaction) domain.Hash {
	h := sha3.NewLegacyKeccak256()
	h.Write(tx.Hash[:])
	writeFastUint(h, tx.Index)
	status := byte(0)
	if tx.Successful {
		status = 1
	}
	h.Write([]byte{status})
	writeFastUint(h, uint64(len(tx.Logs)))
	for _, log := range tx.Logs {
		h.Write(log.Address[:])
		writeFastUint(h, uint64(len(log.Topics)))
		for _, topic := range log.Topics {
			h.Write(topic[:])
		}
		writeFastUint(h, uint64(len(log.Data)))
		h.Write(log.Data)
	}
	var result domain.Hash
	copy(result[:], h.Sum(nil))
	return result
}

func extendFastPrefix(previous domain.Hash, position fastPosition, digest domain.Hash) domain.Hash {
	h := sha3.NewLegacyKeccak256()
	h.Write(previous[:])
	writeFastUint(h, position.block)
	writeFastUint(h, position.index)
	h.Write(digest[:])
	var result domain.Hash
	copy(result[:], h.Sum(nil))
	return result
}

func writeFastUint(h hash.Hash, value uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	h.Write(data[:])
}

func fastMessageBytes(message feed.FastMessage) int {
	size := len(message.RawTx) + 256
	for _, log := range message.Receipt.Logs {
		size += 20 + 32*len(log.Topics) + len(log.Data)
	}
	return size
}

func copyFastMessage(message feed.FastMessage) feed.FastMessage {
	message.RawTx = slices.Clone(message.RawTx)
	message.Receipt.Logs = slices.Clone(message.Receipt.Logs)
	for i := range message.Receipt.Logs {
		log := &message.Receipt.Logs[i]
		log.Topics = slices.Clone(log.Topics)
		log.Data = slices.Clone(log.Data)
	}
	if message.Receipt.EffectiveGasPrice != nil {
		message.Receipt.EffectiveGasPrice = new(big.Int).Set(message.Receipt.EffectiveGasPrice)
	}
	if message.Receipt.BaseFee != nil {
		message.Receipt.BaseFee = new(big.Int).Set(message.Receipt.BaseFee)
	}
	return message
}
