package ingest

import (
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// ReceiptLogs materializes owned canonical logs for background indexing.
func ReceiptLogs(ref domain.BlockRef, transactions []Transaction) []rpc.Log {
	var logs []rpc.Log
	for _, tx := range transactions {
		for _, log := range tx.Logs {
			logs = append(logs, rpc.Log{Address: log.Address, Topics: slices.Clone(log.Topics), Data: slices.Clone(log.Data), BlockNumber: ref.Number, BlockHash: ref.Hash, TxHash: tx.Hash, LogIndex: uint32(len(logs))})
		}
	}
	return logs
}
