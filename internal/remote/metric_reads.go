package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/state"
)

// metricRetryBlocks is how long a MetricOmm pool whose read failed waits
// before the next one, as tickRepair does: a pool that cannot be read must
// not cost a failing call every block.
const metricRetryBlocks = 4

// metricRead is what the follower read for one MetricOmm pool at a canonical
// block. words is nil when only the oracle's word was read.
type metricRead struct {
	key    domain.PoolKey
	words  []domain.Hash
	skew   [3]domain.Hash
	oracle domain.Hash
	// reference holds the words of the pool's two reference feeds, read with
	// every read of a pool registered with a reference oracle.
	reference  [2]domain.Hash
	referenced bool
}

// metricReads reads MetricOmm pools at the canonical block being published,
// like canonical fees: the follower does the network read and the pool writer
// applies it after the block's own logs, so the read is never older than the
// state it replaces and one pool is read at most once per block.
//
// A pool is read in full (its words through extsload, the quote extension's
// three words, the oracle's word and the reference feeds' words) when its
// state is stale or the block
// carries one of its logs: every pool event leaves the log-driven state stale
// (metricomm.FollowLog), in practice a swap, typically a few hundred an hour
// over both pools. Only the oracle's word is read when the block updates
// the oracle in a transaction without calldata, which is a block read as
// receipts; stream blocks carry the calldata and need no read. Nothing here
// is paced or capped: each read restores a pool the search cannot quote.
type metricReads struct {
	client *rpc.Client
	// retryAt holds, per pool, the first block that may retry a failed read.
	retryAt map[domain.PoolKey]uint64
}

// due splits the pools that need a read at a block with these transactions.
func (m *metricReads) due(previous []pool.State, block uint64, transactions []ingest.Transaction) (full, quote []pool.State) {
	for _, p := range previous {
		if p.Metric == nil || block < m.retryAt[p.Key] {
			continue
		}
		touched, repriced := false, false
		for _, tx := range transactions {
			for _, log := range tx.Logs {
				touched = touched || log.Address == p.Key.Address
				repriced = repriced || len(tx.Raw) == 0 && (log.Address == p.Metric.Oracle || log.Address == p.Metric.Reference) && len(log.Topics) != 0 && log.Topics[0] == metricomm.TopicOracleUpdated
			}
		}
		switch {
		// SkewApprox: a swap's read failed or was never made (a Fast swap
		// whose block was applied without one); the old extension state must
		// not price the pool for longer than the retry.
		case p.Metric.Stale || p.Metric.SkewApprox || touched:
			full = append(full, p)
		case repriced:
			quote = append(quote, p)
		}
	}
	return full, quote
}

// read returns the reads due at block. A failed read is logged and left out:
// the pool stays stale or loses its quote, which only removes its edges, and
// is read again metricRetryBlocks later.
func (m *metricReads) read(ctx context.Context, previous []pool.State, block rpc.ExecutionBlock, transactions []ingest.Transaction) []metricRead {
	full, quote := m.due(previous, block.Ref.Number, transactions)
	reads := make([]metricRead, 0, len(full)+len(quote))
	for i, p := range append(full, quote...) {
		read, err := readMetric(ctx, m.client, p, block.Ref.Hash, i < len(full))
		// A stream block can be sealed before the RPC serves it, and a
		// load-balanced backend can trail the one that served the header.
		// The pool is unquotable until this read lands, so wait for the
		// block as the fee reads do, within the same bound.
		for attempt := 0; err != nil && rpc.IsBlockUnavailable(err) && attempt < laggingBackendRetries; attempt++ {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(laggingBackendWait):
			}
			read, err = readMetric(ctx, m.client, p, block.Ref.Hash, i < len(full))
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if m.retryAt == nil {
				m.retryAt = make(map[domain.PoolKey]uint64)
			}
			m.retryAt[p.Key] = block.Ref.Number + metricRetryBlocks
			// Only this file's own messages: a provider error may quote the endpoint.
			slog.Warn("MetricOmm pool read deferred", "pool", p.Key.Address.Hex(), "block", block.Ref.Number, "full", i < len(full), "rate_limited", rpc.IsRateLimited(err))
			continue
		}
		delete(m.retryAt, p.Key)
		reads = append(reads, read)
	}
	return reads
}

// readMetric reads one pool at the block with this hash in a single batch.
func readMetric(ctx context.Context, client *rpc.Client, p pool.State, hash domain.Hash, full bool) (metricRead, error) {
	at := map[string]any{"blockHash": hash.Hex(), "requireCanonical": true}
	storage := func(address domain.Address, slot domain.Hash) rpc.Call {
		return rpc.Call{Method: "eth_getStorageAt", Params: []any{address.Hex(), slot.Hex(), at}}
	}
	calls := []rpc.Call{storage(p.Metric.Oracle, p.Metric.OracleSlot)}
	wordCount := 0
	if full {
		data, count, err := metricomm.WordsCall(p.Metric)
		if err != nil {
			return metricRead{}, err
		}
		wordCount = count
		for _, slot := range metricomm.SkewSlots(p.Metric) {
			calls = append(calls, storage(p.Metric.QuoteExtension, slot))
		}
		calls = append(calls, rpc.Call{Method: "eth_call", Params: []any{map[string]any{"to": p.Key.Address.Hex(), "data": flashHex(data)}, at}})
	}
	referenced := p.Metric.Reference != (domain.Address{})
	if referenced {
		calls = append(calls, storage(p.Metric.Reference, metricomm.ReferenceSlot(p.Metric.ReferenceBaseFeed)), storage(p.Metric.Reference, metricomm.ReferenceSlot(p.Metric.ReferenceQuoteFeed)))
	}
	results, err := client.Batch(ctx, calls)
	if err != nil {
		return metricRead{}, err
	}
	outputs := make([][]byte, len(results))
	for i, result := range results {
		if result.Error != nil {
			return metricRead{}, result.Error
		}
		var text string
		if err := json.Unmarshal(result.Result, &text); err != nil {
			return metricRead{}, err
		}
		if outputs[i], err = hexData(text); err != nil {
			return metricRead{}, err
		}
	}
	read := metricRead{key: p.Key, referenced: referenced}
	if read.oracle, err = storageWord(outputs[0]); err != nil {
		return metricRead{}, err
	}
	if referenced {
		for i := range read.reference {
			if read.reference[i], err = storageWord(outputs[len(outputs)-2+i]); err != nil {
				return metricRead{}, err
			}
		}
	}
	if !full {
		return read, nil
	}
	for i := range read.skew {
		if read.skew[i], err = storageWord(outputs[1+i]); err != nil {
			return metricRead{}, err
		}
	}
	read.words, err = metricomm.DecodeWords(outputs[4], wordCount)
	return read, err
}

// storageWord left-pads an eth_getStorageAt answer; some nodes trim zeros.
func storageWord(raw []byte) (domain.Hash, error) {
	if len(raw) > 32 {
		return domain.Hash{}, fmt.Errorf("invalid storage word")
	}
	var word domain.Hash
	copy(word[32-len(raw):], raw)
	return word, nil
}

// applyMetricReads writes the block's reads into the store after its logs
// and returns the pools that changed. A read older than the pool's last
// change is dropped, and so is one whose words do not describe a pool; the
// pool then stays as the logs left it.
func applyMetricReads(store *state.Store, reads []metricRead, ref domain.BlockRef) ([]domain.PoolKey, error) {
	var changed []domain.PoolKey
	for _, read := range reads {
		current, ok := store.Pool(read.key)
		if !ok || current.Metric == nil || current.UpdatedBlock > ref.Number {
			continue
		}
		if read.referenced {
			metricomm.FollowReferenceWords(current.Metric, read.reference[0], read.reference[1])
		}
		if read.words == nil {
			metricomm.FollowOracleWord(current.Metric, read.oracle, ref.Timestamp)
		} else if err := metricomm.Refresh(current.Metric, read.words, read.skew, read.oracle, ref.Timestamp); err != nil {
			slog.Warn("MetricOmm pool read does not describe a pool", "pool", read.key.Address.Hex(), "block", ref.Number, "error", err)
			continue
		}
		current.UpdatedBlock = ref.Number
		if err := store.Upsert(current); err != nil {
			return nil, err
		}
		changed = append(changed, read.key)
	}
	return changed, nil
}
