package remote

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

const (
	metricWETHPool = "0x2b4a9de7bdaffea997f5e12d0ebbffddf6aea3d3"
	// The record slot and the oracle slot of the WETH/USDC pool, as the
	// catalog configures them, read off traces of the live pool.
	metricRecordSlot = "0xd23040c859ccb3f30ff05a5fa4ef2f6317f4a5baf1ef9c66778e0a50a92baada"
	metricOracleSlot = "0x8dfdc825dd0453e7c80b1c1d32ae910e35d6efe9000000000000000000000000"
	metricExtension  = "0xf1463419c4ac04aa1ac7fcc7b7930e1f61844c16"
)

// metricFixture is the real chain data the MetricOmm tests share: one pool's
// storage words before and after a block with a single swap, the block's
// logs, and a block where the chain's own quote extension was traced.
type metricFixture struct {
	pool, oracle, extension  domain.Address
	registration             pool.MetricState
	before, after            []domain.Hash
	logs                     []ingest.Log
	config, parameters       domain.Hash
	slot0, skewState, update domain.Hash // of the quoted block
	blockTime                uint64
	bid, ask                 string
}

func metricWord(t *testing.T, text string) domain.Hash {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(text, "0x"))
	if err != nil || len(raw) != 32 {
		t.Fatalf("word %q: %v", text, err)
	}
	return domain.Hash(raw)
}

func metricAddress(t *testing.T, text string) domain.Address {
	t.Helper()
	address, err := domain.ParseAddress(text)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func loadMetricFixture(t *testing.T) metricFixture {
	t.Helper()
	read := func(name string, out any) {
		raw, err := os.ReadFile("../dex/metricomm/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
	}
	var events struct {
		Immutables struct {
			Scale0  string `json:"token0_scale_multiplier"`
			Scale1  string `json:"token1_scale_multiplier"`
			Lowest  int16  `json:"lowest_bin"`
			Highest int16  `json:"highest_bin"`
		} `json:"immutables"`
		Cases []struct {
			BlockNumber uint64   `json:"block_number"`
			Before      []string `json:"words_before"`
			Logs        []struct {
				Topics []string `json:"topics"`
				Data   string   `json:"data"`
			} `json:"logs"`
			Changed map[string]string `json:"words_changed"`
		} `json:"cases"`
	}
	read(metricWETHPool+"-events.json", &events)
	var quotes struct {
		Oracle    string `json:"oracle"`
		Extension map[string]struct {
			Config     string `json:"config_word"`
			Parameters string `json:"parameters_word"`
		} `json:"quote_extension"`
		Samples []struct {
			Pool      string `json:"pool"`
			BlockTime uint64 `json:"block_timestamp"`
			Oracle    string `json:"oracle_word"`
			State     string `json:"extension_state_word"`
			Slot0     string `json:"slot0"`
			Bid       string `json:"extension_bid_x64"`
			Ask       string `json:"extension_ask_x64"`
		} `json:"samples"`
	}
	read("quotes.json", &quotes)

	f := metricFixture{pool: metricAddress(t, metricWETHPool), oracle: metricAddress(t, quotes.Oracle), extension: metricAddress(t, metricExtension)}
	scale0, _ := new(big.Int).SetString(events.Immutables.Scale0, 10)
	scale1, _ := new(big.Int).SetString(events.Immutables.Scale1, 10)
	f.registration = pool.MetricState{
		Token0Scale: scale0, Token1Scale: scale1, LowestBin: events.Immutables.Lowest, HighestBin: events.Immutables.Highest,
		Oracle: f.oracle, FeedIndex: 1, OracleSlot: metricWord(t, metricOracleSlot),
		QuoteExtension: f.extension, QuoteExtensionSlot: metricWord(t, metricRecordSlot),
	}
	for _, c := range events.Cases {
		if c.BlockNumber != 523322011 { // one BinSwapped and one Swap
			continue
		}
		for _, word := range c.Before {
			f.before = append(f.before, metricWord(t, word))
		}
		f.after = append([]domain.Hash(nil), f.before...)
		for index, word := range c.Changed {
			i, err := strconv.Atoi(index)
			if err != nil {
				t.Fatal(err)
			}
			f.after[i] = metricWord(t, word)
		}
		for _, log := range c.Logs {
			entry := ingest.Log{Address: f.pool}
			for _, topic := range log.Topics {
				entry.Topics = append(entry.Topics, metricWord(t, topic))
			}
			data, err := hex.DecodeString(strings.TrimPrefix(log.Data, "0x"))
			if err != nil {
				t.Fatal(err)
			}
			entry.Data = data
			f.logs = append(f.logs, entry)
		}
	}
	if len(f.before) != 42 || len(f.logs) != 2 || bytes.Equal(f.before[0][:], f.after[0][:]) {
		t.Fatalf("swap block not in the fixture: %d words, %d logs", len(f.before), len(f.logs))
	}
	words := quotes.Extension[metricWETHPool]
	f.config, f.parameters = metricWord(t, words.Config), metricWord(t, words.Parameters)
	for _, sample := range quotes.Samples {
		if sample.Pool == metricWETHPool {
			f.slot0, f.skewState, f.update = metricWord(t, sample.Slot0), metricWord(t, sample.State), metricWord(t, sample.Oracle)
			f.blockTime, f.bid, f.ask = sample.BlockTime, sample.Bid, sample.Ask
			break
		}
	}
	if f.bid == "" {
		t.Fatal("no quote sample for the pool")
	}
	return f
}

func (f metricFixture) key() domain.PoolKey {
	return domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: f.pool}
}

// state is the pool as bootstrap registers it: stale, nothing read.
func (f metricFixture) state() pool.State {
	return pool.State{Key: f.key(), Token0: addressAt(1), Token1: addressAt(2), Metric: metricomm.Unloaded(&f.registration)}
}

// metricChain is a fake node holding the storage of the pool, its quote
// extension and the oracle. It answers extsload on the pool and
// eth_getStorageAt elsewhere, only at the block hash it is set to.
type metricChain struct {
	t       *testing.T
	fixture metricFixture
	mu      sync.Mutex
	hash    domain.Hash
	words   []domain.Hash
	skew    domain.Hash
	oracle  domain.Hash
	calls   map[string]int
	// unavailable answers that many batches as a backend that lacks the block.
	unavailable int
	// broken makes extsload revert.
	broken bool
	// reference holds the reference oracle's words by slot.
	reference map[string]domain.Hash
}

func (c *metricChain) set(hash domain.Hash, words []domain.Hash, skew, oracle domain.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hash, c.words, c.skew, c.oracle = hash, words, skew, oracle
}

// refer serves the reference oracle's words for the registration's two feeds.
func (c *metricChain) refer(base, quote domain.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.fixture.registration
	c.reference = map[string]domain.Hash{metricomm.ReferenceSlot(r.ReferenceBaseFeed).Hex(): base, metricomm.ReferenceSlot(r.ReferenceQuoteFeed).Hex(): quote}
}

func (c *metricChain) taken() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	taken := c.calls
	c.calls = map[string]int{}
	return taken
}

func (c *metricChain) serve(w http.ResponseWriter, r *http.Request) {
	var batch []struct {
		ID     uint64
		Method string
		Params []json.RawMessage
	}
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		c.t.Error("MetricOmm reads must be one batch", err)
		w.WriteHeader(500)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	unavailable := c.unavailable > 0
	if unavailable {
		c.unavailable--
	}
	f := c.fixture
	slots := metricomm.SkewSlots(&f.registration)
	var rows []any
	for _, call := range batch {
		c.calls[call.Method]++
		var at struct {
			BlockHash        string
			RequireCanonical bool
		}
		json.Unmarshal(call.Params[len(call.Params)-1], &at)
		if at.BlockHash != c.hash.Hex() || !at.RequireCanonical {
			c.t.Errorf("%s not pinned to the canonical block: %s", call.Method, call.Params[len(call.Params)-1])
		}
		failure := func(message string) {
			rows = append(rows, map[string]any{"id": call.ID, "error": map[string]any{"code": -32000, "message": message}})
		}
		if unavailable {
			failure("header not found")
			continue
		}
		var word domain.Hash
		switch call.Method {
		case "eth_getStorageAt":
			var address, slot string
			json.Unmarshal(call.Params[0], &address)
			json.Unmarshal(call.Params[1], &slot)
			switch {
			case address == f.oracle.Hex() && slot == f.registration.OracleSlot.Hex():
				word = c.oracle
			case address == f.extension.Hex() && slot == slots[0].Hex():
				word = f.config
			case address == f.extension.Hex() && slot == slots[1].Hex():
				word = f.parameters
			case address == f.extension.Hex() && slot == slots[2].Hex():
				word = c.skew
			case c.reference != nil && address == f.registration.Reference.Hex():
				word = c.reference[slot]
			default:
				c.t.Errorf("unexpected storage read %s %s", address, slot)
			}
			rows = append(rows, map[string]any{"id": call.ID, "result": word.Hex()})
		case "eth_call":
			var request struct{ To, Data string }
			json.Unmarshal(call.Params[0], &request)
			want, count, err := metricomm.WordsCall(&f.registration)
			if err != nil || request.To != f.pool.Hex() || request.Data != flashHex(want) {
				c.t.Errorf("unexpected call to %s", request.To)
			}
			if c.broken {
				failure("execution reverted")
				continue
			}
			answer := wordData(32, int64(count))
			for _, stored := range c.words {
				answer = append(answer, stored[:]...)
			}
			rows = append(rows, map[string]any{"id": call.ID, "result": flashHex(answer)})
		default:
			c.t.Errorf("unexpected method %s", call.Method)
		}
	}
	json.NewEncoder(w).Encode(rows)
}

func newMetricChain(t *testing.T, f metricFixture) (*metricChain, *rpc.Client) {
	t.Helper()
	chain := &metricChain{t: t, fixture: f, calls: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(chain.serve))
	t.Cleanup(server.Close)
	return chain, rpc.NewClient(server.URL, rpc.Options{})
}

// metricBlock applies one canonical block the way the pool writer does: the
// follower's reads are taken against the previous pools first, the block's
// logs are applied, then the reads.
func metricBlock(t *testing.T, store *poolstate.Store, reads *metricReads, chain *metricChain, timestamp uint64, transactions ...ingest.Transaction) (*poolstate.Store, []domain.PoolKey) {
	t.Helper()
	parent := store.Block()
	ref := domain.BlockRef{Number: parent.Number + 1, Hash: domain.Hash{byte(parent.Number + 1), 0xbb}, ParentHash: parent.Hash, Timestamp: timestamp}
	chain.mu.Lock()
	chain.hash = ref.Hash
	chain.mu.Unlock()
	read := reads.read(t.Context(), store.Pools(), rpc.ExecutionBlock{Ref: ref}, transactions)
	branch, err := ingest.NewTransactionBranch(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.Begin(ref); err != nil {
		t.Fatal(err)
	}
	var hashes []domain.Hash
	for i, tx := range transactions {
		tx.Index, tx.Successful = uint64(i), true
		if _, err := branch.Apply(t.Context(), tx); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, tx.Hash)
	}
	if err := branch.Commit(ref, hashes); err != nil {
		t.Fatal(err)
	}
	next := branch.Base()
	changed, err := applyMetricReads(next, read, ref)
	if err != nil {
		t.Fatal(err)
	}
	return next, changed
}

func metricStore(t *testing.T, pools ...pool.State) *poolstate.Store {
	t.Helper()
	store := poolstate.New()
	for _, p := range pools {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.BeginBlock(domain.BlockRef{Number: 10, Hash: domain.Hash{10}, ParentHash: domain.Hash{9}}); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	store.ClearDirty()
	return store
}

func metricQuote(t *testing.T, store *poolstate.Store, key domain.PoolKey) (*big.Int, error) {
	t.Helper()
	p, ok := store.Pool(key)
	if !ok {
		t.Fatal("pool missing")
	}
	return metricomm.New().QuoteExactIn(t.Context(), p, pool.DeepState{}, p.Token0, big.NewInt(1e15))
}

// oracleTransaction is an update of the oracle: its event without a price
// and, when raw is set, the signed transaction whose calldata carries it.
func oracleTransaction(t *testing.T, f metricFixture, word domain.Hash, raw bool, id byte) ingest.Transaction {
	t.Helper()
	tx := ingest.Transaction{Hash: domain.Hash{id, 0xaa}, Logs: []ingest.Log{{Address: f.oracle, Topics: []domain.Hash{metricomm.TopicOracleUpdated}}}}
	if raw {
		target := common.Address(f.oracle)
		encoded, err := types.NewTx(&types.DynamicFeeTx{ChainID: big.NewInt(42161), To: &target, Data: append([]byte{0}, word[:]...), Gas: 100_000, GasFeeCap: big.NewInt(1), GasTipCap: big.NewInt(1)}).MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		tx.Raw = encoded
	}
	return tx
}

// A registered pool is read at its first canonical block and quotes what the
// chain's own quote extension returned in that block (Arbitrum One block
// 523013135): slot 0, the extension's state word and the oracle's word are
// that block's, the bins are those of the swap fixture.
func TestMetricPoolIsReadAtACanonicalBlockAndQuotesAsTheChain(t *testing.T) {
	f := loadMetricFixture(t)
	chain, client := newMetricChain(t, f)
	words := append([]domain.Hash{f.slot0}, f.before[1:]...)
	chain.set(domain.Hash{}, words, f.skewState, f.update)
	store := metricStore(t, f.state())
	if _, err := metricQuote(t, store, f.key()); !errors.Is(err, metricomm.ErrStaleState) {
		t.Fatalf("an unread pool quoted: %v", err)
	}

	reads := &metricReads{client: client}
	store, changed := metricBlock(t, store, reads, chain, f.blockTime)
	if len(changed) != 1 || changed[0] != f.key() {
		t.Fatalf("the read pool must be searched again: %v", changed)
	}
	if taken := chain.taken(); taken["eth_call"] != 1 || taken["eth_getStorageAt"] != 4 {
		t.Fatalf("one extsload and four storage words expected: %v", taken)
	}
	p, _ := store.Pool(f.key())
	if p.Metric.Stale || p.UpdatedBlock != 11 || p.Metric.BidX64.String() != f.bid || p.Metric.AskX64.String() != f.ask {
		t.Fatalf("quote [%v, %v] at block %d, chain [%s, %s]", p.Metric.BidX64, p.Metric.AskX64, p.UpdatedBlock, f.bid, f.ask)
	}
	if out, err := metricQuote(t, store, f.key()); err != nil || out.Sign() <= 0 {
		t.Fatalf("loaded pool does not quote: %v %v", out, err)
	}
	// Nothing changed: the next block reads nothing.
	store, changed = metricBlock(t, store, reads, chain, f.blockTime)
	if taken := chain.taken(); len(taken) != 0 || len(changed) != 0 {
		t.Fatalf("a current pool was read again: %v %v", taken, changed)
	}
	if p, _ := store.Pool(f.key()); p.Metric.Stale || p.UpdatedBlock != 11 {
		t.Fatal("an idle block changed the pool")
	}
}

// A swap is replayed and the pool keeps quoting, with the extension's old
// state at the new position (SkewApprox; holding the pool instead would lose
// every burst). The block that carries the swap is read once and the pool
// then quotes with the chain's words and extension state after that block.
func TestMetricPoolIsRepairedInTheBlockOfItsSwap(t *testing.T) {
	f := loadMetricFixture(t)
	chain, client := newMetricChain(t, f)
	chain.set(domain.Hash{}, f.before, f.skewState, f.update)
	reads := &metricReads{client: client}
	store, _ := metricBlock(t, metricStore(t, f.state()), reads, chain, f.blockTime)
	chain.taken()

	// Without a read the swapped pool still quotes, on the replayed words.
	unrepaired, _ := metricBlock(t, store, &metricReads{client: client, retryAt: map[domain.PoolKey]uint64{f.key(): 1 << 62}}, chain, f.blockTime, ingest.Transaction{Hash: domain.Hash{1}, Logs: f.logs})
	if _, err := metricQuote(t, unrepaired, f.key()); err != nil {
		t.Fatalf("a swapped pool without a read does not quote: %v", err)
	}
	if p, _ := unrepaired.Pool(f.key()); !sameWords(p.Metric.Words, f.after) || !p.Metric.SkewApprox || p.Metric.Stale {
		t.Fatal("the log replay does not give the chain's words")
	}
	chain.taken()

	// The node now holds the words after the swap and a moved extension state.
	moved := f.skewState
	moved[31] ^= 1
	chain.set(domain.Hash{}, f.after, moved, f.update)
	store, changed := metricBlock(t, store, reads, chain, f.blockTime, ingest.Transaction{Hash: domain.Hash{1}, Logs: f.logs})
	if taken := chain.taken(); taken["eth_call"] != 1 || taken["eth_getStorageAt"] != 4 {
		t.Fatalf("the swap block must be read exactly once: %v", taken)
	}
	p, _ := store.Pool(f.key())
	if len(changed) != 1 || p.Metric.Stale || p.UpdatedBlock != 12 || !sameWords(p.Metric.Words, f.after) || p.Metric.SkewState != moved || p.Metric.BidX64 == nil {
		t.Fatalf("pool not repaired: stale %v, block %d, changed %v", p.Metric.Stale, p.UpdatedBlock, changed)
	}
	if out, err := metricQuote(t, store, f.key()); err != nil || out.Sign() <= 0 {
		t.Fatalf("repaired pool does not quote: %v %v", out, err)
	}

	// A read taken at an older block than the pool's last change is dropped.
	old := metricRead{key: f.key(), words: f.before, oracle: f.update}
	if changed, err := applyMetricReads(store, []metricRead{old}, domain.BlockRef{Number: 11, Timestamp: f.blockTime}); err != nil || len(changed) != 0 {
		t.Fatalf("an older read was applied: %v %v", changed, err)
	}
	if p, _ := store.Pool(f.key()); !sameWords(p.Metric.Words, f.after) {
		t.Fatal("an older read replaced newer words")
	}
}

func sameWords(a, b []domain.Hash) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An oracle update prices the pool from its calldata. A block read as
// receipts has none, so the oracle's word is read, alone; a stream block
// carries the calldata and costs no read.
func TestMetricOracleUpdateReadsOnlyWithoutCalldata(t *testing.T) {
	f := loadMetricFixture(t)
	chain, client := newMetricChain(t, f)
	chain.set(domain.Hash{}, f.before, f.skewState, f.update)
	reads := &metricReads{client: client}
	store, _ := metricBlock(t, metricStore(t, f.state()), reads, chain, f.blockTime)
	chain.taken()
	first, _ := store.Pool(f.key())

	// The same update a second later: the timestamp is in bytes 23..30 of the word.
	later := f.update
	later[30] ^= 0x10
	chain.set(domain.Hash{}, f.before, f.skewState, later)
	store, changed := metricBlock(t, store, reads, chain, f.blockTime, oracleTransaction(t, f, later, false, 1))
	if taken := chain.taken(); taken["eth_getStorageAt"] != 1 || taken["eth_call"] != 0 {
		t.Fatalf("an update without calldata needs the oracle's word alone: %v", taken)
	}
	p, _ := store.Pool(f.key())
	if len(changed) != 1 || p.Metric.BidX64 == nil || p.Metric.QuoteMS == first.Metric.QuoteMS || p.Metric.BidX64.Cmp(first.Metric.BidX64) != 0 {
		t.Fatalf("quote not taken from the oracle's word: %v at %d", p.Metric.BidX64, p.Metric.QuoteMS)
	}

	store, _ = metricBlock(t, store, reads, chain, f.blockTime, oracleTransaction(t, f, f.update, true, 2))
	if taken := chain.taken(); len(taken) != 0 {
		t.Fatalf("an update with calldata was read from the node: %v", taken)
	}
	if p, _ := store.Pool(f.key()); p.Metric.BidX64 == nil || p.Metric.QuoteMS != first.Metric.QuoteMS {
		t.Fatalf("quote not taken from the calldata: %v at %d", p.Metric.BidX64, p.Metric.QuoteMS)
	}
}

// A backend that does not have the block yet is waited for, since the pool
// cannot be quoted until the read lands. A read that fails otherwise is not
// repeated every block.
func TestMetricReadWaitsForTheBlockAndBacksOffAFailure(t *testing.T) {
	f := loadMetricFixture(t)
	chain, client := newMetricChain(t, f)
	chain.set(domain.Hash{}, f.before, f.skewState, f.update)
	chain.unavailable = 3
	reads := &metricReads{client: client}
	store, changed := metricBlock(t, metricStore(t, f.state()), reads, chain, f.blockTime)
	if p, _ := store.Pool(f.key()); len(changed) != 1 || p.Metric.Stale {
		t.Fatal("the read did not wait for the block")
	}
	chain.taken()

	chain.broken = true
	store, _ = metricBlock(t, store, reads, chain, f.blockTime, ingest.Transaction{Hash: domain.Hash{1}, Logs: f.logs})
	// The swap is replayed; only the extension's state waits for the read.
	if p, _ := store.Pool(f.key()); p.Metric.Stale || !p.Metric.SkewApprox {
		t.Fatal("a failed read after a swap did not leave the replayed pool with an approximate skew")
	}
	chain.taken()
	for range metricRetryBlocks - 1 {
		store, _ = metricBlock(t, store, reads, chain, f.blockTime)
	}
	if taken := chain.taken(); len(taken) != 0 {
		t.Fatalf("a failed read was repeated before its retry block: %v", taken)
	}
	chain.broken = false
	chain.set(domain.Hash{}, f.after, f.skewState, f.update)
	store, _ = metricBlock(t, store, reads, chain, f.blockTime)
	// The approximate skew must not outlive the backoff: the retry reads the
	// extension's state although no new log touches the pool.
	if p, _ := store.Pool(f.key()); p.Metric.Stale || p.Metric.SkewApprox || !sameWords(p.Metric.Words, f.after) {
		t.Fatal("the pool was not read again at its retry block")
	}
}

// A pool registered with a reference oracle reads its two feeds in the same
// batch and quotes inside the provider's band: with the reference 1% above
// the fixture's quote the ask is the band's, and with the feeds unset on
// chain the pool has no quote.
func TestMetricPoolReadsItsReferenceFeeds(t *testing.T) {
	f := loadMetricFixture(t)
	f.registration.Reference, f.registration.ReferenceBaseFeed, f.registration.ReferenceQuoteFeed = addressAt(77), 2, 7
	chain, client := newMetricChain(t, f)
	words := append([]domain.Hash{f.slot0}, f.before[1:]...)
	chain.set(domain.Hash{}, words, f.skewState, f.update)
	ask, _ := new(big.Int).SetString(f.ask, 10)
	// The chain's ask in whole tokens, 1% up, as the base feed over $1.
	base := new(big.Int).Mul(ask, big.NewInt(101_000_000))
	base.Rsh(base, 64)
	word := func(price uint64) domain.Hash {
		var w domain.Hash
		binary.BigEndian.PutUint64(w[24:], price)
		binary.BigEndian.PutUint64(w[12:], 1_794_580_243_516)
		return w
	}
	chain.refer(word(base.Uint64()), word(100_000_000))

	reads := &metricReads{client: client}
	store, changed := metricBlock(t, metricStore(t, f.state()), reads, chain, f.blockTime)
	if taken := chain.taken(); len(changed) != 1 || taken["eth_call"] != 1 || taken["eth_getStorageAt"] != 6 {
		t.Fatalf("one batch with six storage words expected: %v, changed %v", taken, changed)
	}
	p, _ := store.Pool(f.key())
	_, band := metricomm.ReferenceBand(base.Uint64(), 100_000_000)
	if p.Metric.BidX64.String() != f.bid || p.Metric.AskX64.Cmp(band) != 0 || band.Cmp(ask) <= 0 {
		t.Fatalf("quote [%v, %v], chain bid %s, band ask %s", p.Metric.BidX64, p.Metric.AskX64, f.bid, band)
	}

	chain.refer(domain.Hash{}, domain.Hash{})
	unset := metricStore(t, f.state())
	unset, _ = metricBlock(t, unset, reads, chain, f.blockTime)
	if p, _ := unset.Pool(f.key()); p.Metric.Stale || p.Metric.BidX64 != nil {
		t.Fatalf("quoted without reference feeds: %v", p.Metric.BidX64)
	}
}
