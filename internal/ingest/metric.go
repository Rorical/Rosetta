package ingest

import (
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// applyMetric replays one log of a MetricOmm pool. The pool is marked changed
// only when the log is one of the pool's own events.
func applyMetric(store *state.Store, log Log, key domain.PoolKey, block uint64) error {
	current, ok := store.Pool(key)
	if !ok || current.Metric == nil {
		return state.ErrPoolNotFound
	}
	if !metricomm.FollowLog(current.Metric, log.Topics, log.Data, store.Block().Timestamp) {
		return nil
	}
	current.UpdatedBlock = block
	return store.Upsert(current)
}

// applyMetricOracle gives the pools priced by oracle the quote of an update
// transaction, and the pools held to it as their reference its feeds. The
// update's event carries no price, so both come from the calldata; a
// transaction seen without it (a block read as receipts) clears the quote
// instead of leaving the previous one standing.
func applyMetricOracle(store *state.Store, oracle domain.Address, tx Transaction, ref domain.BlockRef) {
	data, direct := directCall(oracle, tx.Raw)
	update, err := metricomm.DecodeOracleUpdate(data)
	reference, referenceErr := metricomm.DecodeReferenceUpdate(data)
	store.UpdateMetric(oracle, ref.Number, func(metric *pool.MetricState) {
		if metric.Oracle == oracle {
			if direct && err == nil {
				metricomm.FollowOracle(metric, update, ref.Timestamp)
			} else {
				metricomm.ForgetQuote(metric)
			}
		}
		if metric.Reference == oracle {
			if direct && referenceErr == nil {
				metricomm.FollowReference(metric, reference, ref.Timestamp)
			} else {
				metricomm.ForgetReference(metric)
			}
		}
	})
}

// directCall returns the calldata of raw when it is a direct call of the
// oracle. An update made through another contract is not decoded: its
// calldata is that contract's.
func directCall(oracle domain.Address, raw []byte) ([]byte, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var signed types.Transaction
	if err := signed.UnmarshalBinary(raw); err != nil || signed.To() == nil || domain.Address(*signed.To()) != oracle {
		return nil, false
	}
	return signed.Data(), true
}
