package remote

import (
	"maps"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

// How quietPools keeps the word models (reactive_quiet_words.go) at the head:
// applied blocks move them, a read starts them, and the Fast view's pending
// logs name the pools that are changing and so carry no condition.

// quietBlockRing is how many applied blocks are kept for a pool whose words
// were read a few blocks behind the head.
const quietBlockRing = 64

// quietBlock is one applied block's events on derived kinds.
type quietBlock struct {
	number    uint64
	timestamp uint64
	logs      []ingest.Log
}

// observeBlock follows one applied canonical block. A gap in block numbers
// drops every model: their events were not seen.
func (q *quietPools) observeBlock(ref domain.BlockRef, transactions []ingest.Transaction) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := len(q.recent); n > 0 && q.recent[n-1].number+1 != ref.Number {
		q.dropModels()
	}
	block := quietBlock{number: ref.Number, timestamp: ref.Timestamp}
	for _, transaction := range transactions {
		for _, log := range transaction.Logs {
			if len(log.Topics) != 0 && followedTopic(log.Topics[0]) {
				block.logs = append(block.logs, log)
			}
		}
	}
	q.recent = append(q.recent, block)
	if len(q.recent) > quietBlockRing {
		q.recent = q.recent[len(q.recent)-quietBlockRing:]
	}
	q.follow(block)
	maps.DeleteFunc(q.touched, func(_ wordKey, number uint64) bool { return number <= ref.Number })
}

// observeFast notes the pools that a Fast message's logs change in a block
// that is not applied yet: conditions leaves them without a condition until
// they have rested.
func (q *quietPools) observeFast(number uint64, logs []ingest.Log) {
	var changed []wordKey
	for _, log := range logs {
		if len(log.Topics) != 0 && followedTopic(log.Topics[0]) {
			changed = append(changed, logKey(log))
		}
	}
	if len(changed) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := len(q.recent); n > 0 && number <= q.recent[n-1].number {
		return
	}
	for _, key := range changed {
		q.touched[key] = number
	}
}

// follow applies a block's logs to the models; the caller holds mu.
func (q *quietPools) follow(block quietBlock) {
	for _, log := range block.logs {
		key := logKey(log)
		m := q.models[key]
		if m == nil {
			continue
		}
		if !m.apply(log, block.timestamp) {
			delete(q.models, key)
			q.count("quiet_words_dropped")
		}
	}
}

// dropModels forgets every model and the block ring; the caller holds mu.
func (q *quietPools) dropModels() {
	if len(q.models) > 0 {
		q.count("quiet_words_reset")
	}
	clear(q.models)
	q.recent = nil
	q.warmed, q.waitingSince = false, time.Now()
}

// reset drops the models after the pool store was replaced.
func (q *quietPools) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropModels()
}

// adopt starts a model from words read at block and brings it to the head
// with the ring's later blocks. It reports false when the ring does not
// reach back to the read block or the words do not have the kind's shape. The
// caller holds mu.
func (q *quietPools) adopt(pool domain.PoolKey, slots []execution.SlotCondition, block uint64) bool {
	shape, ok := shapeOf(pool.Kind)
	if !ok {
		return false
	}
	key := modelKey(pool)
	at := -1
	for i, b := range q.recent {
		if b.number == block {
			at = i
		}
	}
	if at < 0 {
		return false
	}
	m := newWordModel(shape, slots, q.recent[at].timestamp)
	if m == nil || (shape == shapeV2 && !q.stampedByLastSync(m, key, at)) {
		// Words without the shape of a pair's, a V4 pool's or a Camelot
		// pool's will not have it at the next read either; warm must not read
		// the pool for a model again and again.
		if shape != shapeV3 {
			q.unfit[key] = true
		}
		return false
	}
	for _, later := range q.recent[at+1:] {
		for _, log := range later.logs {
			if logKey(log) == key && !m.apply(log, later.timestamp) {
				return false
			}
		}
	}
	q.models[key] = m
	return true
}

// quietWarmReads is how many pools one warm call may queue for a first read;
// calls are quietWarmEvery apart while there is something to read and
// quietWarmIdle apart once a pass finds nothing.
const (
	quietWarmReads = 4
	quietWarmEvery = time.Second
	quietWarmIdle  = 30 * time.Second
)

// warmDue reports whether the next warm call is due; the caller then passes
// the pools, which are not worth copying on every loop tick.
func (q *quietPools) warmDue(now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !now.Before(q.warmAt)
}

// warm queues a first read for derived-kind pools that have no model yet, a
// few per call (a pool that never rests is read against its last Swap or
// Sync, swapState), so that a route's first use of a pool
// already finds its words. Started lazily, a model would exist only after a
// route had needed it, and for many minutes after a restart few intents
// could hold a pool. A first read is 16 slots, 2 for a V4 pool, so warming a
// whole catalog costs on the order of $0.10 in RPC calls per start. Pools are
// visited round-robin across calls.
func (q *quietPools) warm(pools []pool.State) {
	now := time.Now()
	q.mu.Lock()
	defer q.mu.Unlock()
	queued := 0
	// A pass that found nothing to read waits longer before the next.
	defer func() {
		q.warmAt = now.Add(quietWarmIdle)
		if queued > 0 {
			q.warmAt = now.Add(quietWarmEvery)
		} else if len(q.queued) == 0 {
			q.warmed = true
		}
	}()
	for range pools {
		if queued == quietWarmReads {
			return
		}
		state := pools[q.warmNext%len(pools)]
		q.warmNext++
		key := state.Key
		if !q.derives(key) || !comparable(state) || q.models[modelKey(key)] != nil || q.queued[key] || now.Before(q.retry[key]) {
			continue
		}
		select {
		case q.requests <- state.Copy():
			q.queued[key] = true
			queued++
			q.count("quiet_warm_read_queued")
		default:
			return
		}
	}
}

// derives reports whether the pool's words are derived from its events: its
// kind has a shape and its words were not found to differ from it. The caller
// holds mu.
func (q *quietPools) derives(key domain.PoolKey) bool {
	return derivedKind(key.Kind) && !q.unfit[modelKey(key)]
}

// swapState is state with the values the pool had at the end of block, taken
// from its last event in the ring up to that block. That event must state
// them whole: a V3, V4 or Camelot Swap gives the price and liquidity, a V2
// Sync both reserves. After a Mint, Burn or ModifyLiquidity the liquidity word
// is not known from the log alone. False when the ring has no such event or does not
// reach the block.
func (q *quietPools) swapState(state pool.State, block uint64) (pool.State, bool) {
	shape, ok := shapeOf(state.Key.Kind)
	if !ok {
		return state, false
	}
	key := modelKey(state.Key)
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := len(q.recent); n == 0 || q.recent[n-1].number < block {
		return state, false
	}
	for i := len(q.recent) - 1; i >= 0; i-- {
		if q.recent[i].number > block {
			continue
		}
		logs := q.recent[i].logs
		for j := len(logs) - 1; j >= 0; j-- {
			if logKey(logs[j]) != key {
				continue
			}
			at := state.Copy()
			switch statedBy(shape, logs[j], &at) {
			case statesAll:
				return at, true
			case statesNothing:
				continue
			}
			return state, false
		}
	}
	return state, false
}

// What a log says of the values a pool's words are checked against.
const (
	statesPart    = iota // changes them without stating them
	statesNothing        // leaves them as they were
	statesAll            // states them; statedBy has set them
)

func statedBy(shape wordShape, log ingest.Log, at *pool.State) int {
	first, second := 2, 3 // price and liquidity in a V3, V4 or Camelot Swap
	switch {
	case shape == shapeV3 && log.Topics[0] == topicCardinalityNext, shape == shapeV4 && log.Topics[0] == topicV4ProtocolFee,
		shape == shapeCamelot && (log.Topics[0] == topicCamelotFee || log.Topics[0] == topicCamelotCommunityFee):
		return statesNothing
	case shape == shapeV3 && (log.Topics[0] == topicSwap || log.Topics[0] == topicSwapPancake), shape == shapeV4 && log.Topics[0] == topicV4Swap,
		shape == shapeCamelot && log.Topics[0] == topicSwap:
	case shape == shapeV2 && log.Topics[0] == topicSync:
		first, second = 0, 1
	default:
		return statesPart
	}
	a, ok := dataWord(log.Data, first)
	b, ok2 := dataWord(log.Data, second)
	if !ok || !ok2 {
		return statesPart
	}
	if shape == shapeV2 {
		at.Reserve0, at.Reserve1 = a, b
	} else {
		at.SqrtPriceX96, at.Liquidity = a, b
	}
	return statesAll
}
