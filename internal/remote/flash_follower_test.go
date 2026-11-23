package remote

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// testChain is a JSON-RPC server for canonical blocks by number and hash.
type testChain struct {
	t       *testing.T
	mu      sync.Mutex
	byHash  map[domain.Hash]testBlock
	number  map[uint64]domain.Hash
	latest  uint64
	methods map[string]int
	// lagging answers this many eth_calls as a load-balanced backend that
	// has not reached the block yet.
	lagging int
}

func newTestChain(t *testing.T, latest uint64, blocks ...testBlock) (*testChain, *httptest.Server) {
	c := &testChain{t: t, byHash: map[domain.Hash]testBlock{}, number: map[uint64]domain.Hash{}, latest: latest, methods: map[string]int{}}
	for _, block := range blocks {
		c.add(block)
	}
	server := httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(server.Close)
	return c, server
}

// add makes block canonical at its height.
func (c *testChain) add(block testBlock) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byHash[block.ref.Hash] = block
	c.number[block.ref.Number] = block.ref.Hash
}

func (c *testChain) set(latest uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latest = latest
}

func (c *testChain) calls(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.methods[method]
}

func (c *testChain) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		c.t.Error(err)
		return
	}
	if len(body) > 0 && body[0] == '[' {
		var calls []testCall
		if err := json.Unmarshal(body, &calls); err != nil {
			c.t.Error(err)
			return
		}
		answers := make([]map[string]any, len(calls))
		for i, call := range calls {
			answers[i] = c.answer(call)
		}
		json.NewEncoder(w).Encode(answers)
		return
	}
	var call testCall
	if err := json.Unmarshal(body, &call); err != nil {
		c.t.Error(err)
		return
	}
	json.NewEncoder(w).Encode(c.answer(call))
}

type testCall struct {
	ID     uint64
	Method string
	Params []json.RawMessage
}

func (c *testChain) answer(call testCall) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.methods[call.Method]++
	var tag string
	if len(call.Params) > 0 {
		json.Unmarshal(call.Params[0], &tag)
	}
	var result any
	switch call.Method {
	case "eth_getBlockByNumber":
		number := c.latest
		if tag != "latest" {
			number, _ = strconv.ParseUint(tag[2:], 16, 64)
		}
		if block, ok := c.byHash[c.number[number]]; ok && number <= c.latest {
			result = block.header()
		}
	case "eth_getBlockByHash":
		hash, _ := domain.ParseHash(tag)
		if block, ok := c.byHash[hash]; ok {
			result = block.header()
		}
	case "eth_getBlockReceipts":
		hash, _ := domain.ParseHash(tag)
		result = c.byHash[hash].receipts()
	case "eth_call":
		if c.lagging > 0 {
			c.lagging--
			return map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32000, "message": "requested block ahead of current block and the hash is not currently canonical"}}
		}
		var at struct{ BlockHash string }
		if len(call.Params) > 1 && json.Unmarshal(call.Params[1], &at) == nil && at.BlockHash != "" {
			if hash, _ := domain.ParseHash(at.BlockHash); c.byHash[hash].ref.Hash != hash {
				return map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32000, "message": "header not found"}}
			}
		}
		result = "0x" + hex.EncodeToString(wordData(1_000_000, 4_000_000, 0, 0, 0, 0))
	default:
		c.t.Errorf("unexpected RPC %s", call.Method)
	}
	return map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}
}

// assemble runs frames through the stream assembler and returns its seals.
type followerRun struct {
	updates  chan canonicalUpdate
	seals    chan flashSeal
	registry *telemetry.Registry
	done     chan error
	cancel   context.CancelFunc
}

// startFollower runs the follower; stream, when set, configures a stream
// that already holds seals.
func startFollower(t *testing.T, url string, head domain.BlockRef, pools []pool.State, stream func(*flashCanonical), seals ...flashSeal) *followerRun {
	t.Helper()
	return startFollowerWith(t, config.Config{}, url, head, pools, stream, seals...)
}

// startFollowerWith runs the follower with base's search settings.
func startFollowerWith(t *testing.T, base config.Config, url string, head domain.BlockRef, pools []pool.State, stream func(*flashCanonical), seals ...flashSeal) *followerRun {
	t.Helper()
	base.NetworkName, base.RPCURL, base.RPCPoll, base.RPCWait = "optimism", url, time.Hour, time.Second
	cfg, err := base.ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	run := &followerRun{updates: make(chan canonicalUpdate), registry: &telemetry.Registry{}, done: make(chan error, 1)}
	var flash *flashCanonical
	if stream != nil {
		run.seals = make(chan flashSeal, 64)
		for _, seal := range seals {
			run.seals <- seal
		}
		flash = newFlashCanonical(run.seals, flashVerifyEvery)
		stream(flash)
	}
	ctx, cancel := context.WithTimeout(telemetry.WithRegistry(t.Context(), run.registry), 5*time.Second)
	run.cancel = cancel
	go func() {
		run.done <- followRPC(ctx, cfg, rpc.NewClient(url, rpc.Options{}), specsFor(pools), head, 1, nil, flash, run.updates, pools)
	}()
	t.Cleanup(func() { cancel(); <-run.done })
	return run
}

func (run *followerRun) next(t *testing.T) canonicalUpdate {
	t.Helper()
	select {
	case update := <-run.updates:
		close(update.accepted)
		return update
	case err := <-run.done:
		run.done <- err // for the cleanup
		t.Fatal("follower stopped: ", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no canonical update")
	}
	return canonicalUpdate{}
}

func (run *followerRun) quiet(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case update := <-run.updates:
		t.Fatal("unexpected canonical update", update.block.Ref, update.reset != nil)
	case <-time.After(wait):
	}
}

func (run *followerRun) count(event string) uint64 {
	return run.registry.Snapshot().Sources["standard"].Counters[event]
}

// trusted configures a stream that verifies the stream block after
// unverified more, with the head check just done.
func trusted(unverified int) func(*flashCanonical) {
	return func(f *flashCanonical) {
		f.probation, f.unverified = 0, flashVerifyEvery-1-unverified
		f.lastCheck = time.Now()
	}
}

// verifiedAfter trusts a stream verified just now, the next check due after
// blocks stream blocks, so every unverified block was seen by the follower.
func verifiedAfter(blocks int) func(*flashCanonical) {
	return func(f *flashCanonical) {
		f.probation, f.unverified, f.verifyEvery = 0, 0, blocks+1
		f.lastCheck = time.Now()
	}
}

// probation configures a new stream, with the head check just done.
func probation(f *flashCanonical) { f.lastCheck = time.Now() }

func sameTransactionsExceptFees(a, b []ingest.Transaction) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		x.Fees, y.Fees = nil, nil
		if !reflect.DeepEqual(normalizeLogs(x), normalizeLogs(y)) {
			return false
		}
	}
	return true
}

func normalizeLogs(tx ingest.Transaction) ingest.Transaction {
	logs := make([]ingest.Log, len(tx.Logs))
	for i, log := range tx.Logs {
		logs[i] = ingest.Log{Address: log.Address, Topics: append([]domain.Hash{}, log.Topics...), Data: append([]byte{}, log.Data...)}
	}
	tx.Logs = logs
	return tx
}

// The same block from the stream and from RPC receipts yields the same update
// content, except the fees only receipts carry, and the same pool state.
func TestStreamAndRPCBlocksProduceTheSameState(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 2, pools)
	_, server := newTestChain(t, 11, blocks...)

	viaRPC := startFollower(t, server.URL, head, pools, nil).next(t)
	stream := startFollower(t, server.URL, head, pools, trusted(5), streamOf(blocks[:1], blocks[1])...)
	viaStream := stream.next(t)

	if viaRPC.block.Ref != viaStream.block.Ref || viaRPC.block.BaseFee.Cmp(viaStream.block.BaseFee) != 0 || viaRPC.block.Coinbase != viaStream.block.Coinbase || !reflect.DeepEqual(viaRPC.block.Transactions, viaStream.block.Transactions) {
		t.Fatal("block identity differs by source", viaRPC.block, viaStream.block)
	}
	if !sameTransactionsExceptFees(viaRPC.transactions, viaStream.transactions) || viaRPC.tip != viaStream.tip || viaRPC.catchingUp != viaStream.catchingUp || viaRPC.nearTip != viaStream.nearTip {
		t.Fatal("update content differs by source")
	}
	if viaRPC.transactions[1].Fees == nil || viaStream.transactions[1].Fees != nil {
		t.Fatal("fees must come from receipts only")
	}
	if viaRPC.block.GasLimit != 30_000_000 || stream.count("canonical_flash") != 1 || stream.count("canonical_rpc") != 0 {
		t.Fatal("stream block not counted as stream block")
	}

	apply := func(update canonicalUpdate) []byte {
		t.Helper()
		store, _ := poolFixture(t)
		r := &runtime{store: store}
		if err := r.acceptCanonical(t.Context(), update); err != nil {
			t.Fatal(err)
		}
		encoded, err := r.store.EncodeSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if !bytes.Equal(apply(viaRPC), apply(viaStream)) {
		t.Fatal("pool state differs by source")
	}
}

func TestTrustedStreamBlocksNeedNoRPCUntilVerificationIsDue(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 4, pools)
	chain, server := newTestChain(t, 13, blocks...)
	run := startFollower(t, server.URL, head, pools, trusted(2), streamOf(blocks[:3], blocks[3])...)
	for i := range 2 {
		update := run.next(t)
		if update.block.Ref != blocks[i].ref || update.transactions[1].Fees != nil {
			t.Fatal("unverified stream block", update.block.Ref)
		}
	}
	verified := run.next(t)
	if verified.block.Ref != blocks[2].ref || verified.transactions[1].Fees == nil {
		t.Fatal("verified block must carry receipt fees")
	}
	// One header and one receipts read for three blocks.
	if chain.calls("eth_getBlockByNumber") != 1 || chain.calls("eth_getBlockByHash") != 0 || chain.calls("eth_getBlockReceipts") != 1 || run.count("canonical_verified") != 1 || run.count("canonical_flash") != 3 {
		t.Fatal("verification cost", chain.methods, run.count("canonical_verified"))
	}
}

func TestNewStreamIsVerifiedBlockByBlock(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 3, pools)
	chain, server := newTestChain(t, 12, blocks...)
	run := startFollower(t, server.URL, head, pools, probation, streamOf(blocks[:2], blocks[2])...)
	run.next(t)
	run.next(t)
	if chain.calls("eth_getBlockReceipts") != 2 || run.count("canonical_verified") != 2 {
		t.Fatal("probation must verify every stream block", chain.methods)
	}
}

// One differing block costs two reads per stream block applied since the last
// verification, not a reseed (about 43,000 reads on Arbitrum One): when
// those blocks match their receipts, the RPC block replaces the stream's.
func TestStreamContentMismatchRechecksUnverifiedBlocks(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 3, pools)
	stream := streamOf(blocks[:2], blocks[2])
	// The RPC block has the stream's hash but another receipt: a block whose
	// last frame never arrived looks like this.
	changed := blocks[1]
	changed.transactions = append(append([]ingest.Transaction{}, changed.transactions...), ingest.Transaction{Hash: domain.Hash{0xaa}, Index: 3, Successful: true})
	chain, server := newTestChain(t, 12, blocks[0], changed)
	run := startFollower(t, server.URL, head, pools, verifiedAfter(1), stream...)
	if update := run.next(t); update.block.Ref != blocks[0].ref || update.reset != nil {
		t.Fatal("unverified block", update.block.Ref)
	}
	update := run.next(t)
	if update.reset != nil || update.block.Ref != changed.ref || len(update.transactions) != len(changed.transactions) || update.transactions[0].Fees == nil {
		t.Fatal("the RPC block must replace a differing stream block after a matching recheck", update.block.Ref, update.reset != nil)
	}
	// The due check reads block 12 by number; the recheck reads 11 by hash.
	if run.count("canonical_mismatch") != 1 || run.count("canonical_rpc") != 1 || chain.calls("eth_getBlockByHash") != 1 || chain.calls("eth_getBlockReceipts") != 2 {
		t.Fatal("recheck cost", chain.methods)
	}
}

func TestStreamContentMismatchReseedsWhenAnUnverifiedBlockDiffers(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 3, pools)
	stream := streamOf(blocks[:2], blocks[2])
	first, second := blocks[0], blocks[1]
	first.transactions = first.transactions[:2]
	second.transactions = append(append([]ingest.Transaction{}, second.transactions...), ingest.Transaction{Hash: domain.Hash{0xaa}, Index: 3, Successful: true})
	_, server := newTestChain(t, 12, first, second)
	run := startFollower(t, server.URL, head, pools, verifiedAfter(1), stream...)
	run.next(t)
	reset := run.next(t)
	if reset.reset == nil || reset.block.Ref != second.ref {
		t.Fatal("a differing unverified block must reseed", reset.block.Ref)
	}
}

func TestStreamContentMismatchOnProbationTakesRPCBlock(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 2, pools)
	stream := streamOf(blocks[:1], blocks[1])
	changed := blocks[0]
	changed.transactions = changed.transactions[:2]
	_, server := newTestChain(t, 11, changed)
	run := startFollower(t, server.URL, head, pools, probation, stream...)
	update := run.next(t)
	if update.reset != nil || len(update.transactions) != 2 || update.transactions[0].Fees == nil {
		t.Fatal("verified predecessors: the RPC block replaces the stream block", len(update.transactions))
	}
	if run.count("canonical_mismatch") != 1 || run.count("canonical_rpc") != 1 || run.count("canonical_flash") != 0 {
		t.Fatal("mismatch counters")
	}
}

func TestForkedStreamBlockFallsBackToRPC(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 2, pools)
	stream := streamOf(blocks[:1], blocks[1])
	canonical := blocks[0]
	canonical.ref.Hash = domain.Hash{0xcc}
	_, server := newTestChain(t, 11, canonical)
	run := startFollower(t, server.URL, head, pools, trusted(0), stream...)
	update := run.next(t)
	if update.reset != nil || update.block.Ref != canonical.ref || update.transactions[0].Fees == nil {
		t.Fatal("RPC sibling not applied", update.block.Ref)
	}
	if run.count("canonical_mismatch") != 1 || run.count("canonical_rpc") != 1 {
		t.Fatal("fork counters")
	}
}

// A block the stream did not deliver whole is read by RPC, and the stream
// resumes at its successor: every height is published once, in order, so the
// settlement watch sees a contiguous read range.
func TestStreamGapIsFilledByRPC(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 3, pools)
	chain, server := newTestChain(t, 12, blocks...)
	stream := streamOf(blocks[:2], blocks[2])
	run := startFollower(t, server.URL, head, pools, trusted(5), flashSeal{number: 11, hash: blocks[0].ref.Hash}, stream[1])
	first, second := run.next(t), run.next(t)
	if first.block.Ref != blocks[0].ref || first.transactions[0].Fees == nil || second.block.Ref != blocks[1].ref || second.reset != nil {
		t.Fatal("gap not filled in order", first.block.Ref, second.block.Ref)
	}
	if chain.calls("eth_getBlockReceipts") != 1 || run.count("canonical_rpc") != 1 || run.count("canonical_flash") != 1 {
		t.Fatal("gap cost", chain.methods)
	}
}

// After the stream supplied block 11, a seal naming another block 11 makes the
// follower recheck RPC, which reorganizes as before.
func TestStreamReorgRechecksRPCAndReseeds(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 2, pools)
	chain, server := newTestChain(t, 10)
	run := startFollower(t, server.URL, head, pools, trusted(5), streamOf(blocks[:1], blocks[1])...)
	if update := run.next(t); update.block.Ref != blocks[0].ref {
		t.Fatal(update.block.Ref)
	}
	replacement := blocks[0]
	replacement.ref.Hash = domain.Hash{0xdd}
	chain.add(replacement)
	chain.set(11)
	run.seals <- flashSeal{number: 11, hash: replacement.ref.Hash}
	reset := run.next(t)
	if reset.reset == nil || reset.block.Ref != replacement.ref {
		t.Fatal("stream reorg not recovered at the RPC head", reset.block.Ref)
	}
}

func TestRPCHeadBehindTheStreamIsNotAReorg(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	blocks := testBlocks(head, 3, pools)
	// The RPC serves block 11 as latest while the stream sealed 12.
	chain, server := newTestChain(t, 11, blocks[0])
	headCheckDue := func(f *flashCanonical) { trusted(5)(f); f.lastCheck = time.Time{} }
	run := startFollower(t, server.URL, head, pools, headCheckDue, streamOf(blocks[:2], blocks[2])...)
	run.next(t)
	run.next(t)
	run.quiet(t, 300*time.Millisecond)
	if chain.calls("eth_getBlockByNumber") == 0 || chain.calls("eth_call") != 0 {
		t.Fatal("head check missing, or a lagging RPC reseeded", chain.methods)
	}
}

// A trusted stream block that the RPC never serves, such as one the chain
// dropped, leaves its height to the RPC path instead of stopping the follower.
func TestStreamBlockTheRPCNeverServesFallsBackToRPC(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	dynamic := pools[0]
	dynamic.Key, dynamic.TickSpacing = domain.PoolKey{Kind: domain.PoolKindCamelotV3, Address: addressAt(30)}, 60
	pools = append(pools, dynamic)
	blocks := testBlocks(head, 2, pools)
	stream := streamOf(blocks[:1], blocks[1])
	canonical := blocks[0]
	canonical.ref.Hash = domain.Hash{0xcc}
	_, server := newTestChain(t, 11, canonical)
	run := startFollowerWith(t, config.Config{SearchConfig: "search.json"}, server.URL, head, pools, trusted(5), stream...)
	update := run.next(t)
	if update.reset != nil || update.block.Ref != canonical.ref || run.count("canonical_rpc") != 1 || run.count("canonical_flash") != 0 {
		t.Fatal("dropped stream block not replaced by the RPC block", update.block.Ref)
	}
}

// A block the RPC served can still be missing on the load-balanced backend
// that answers the next read; its fee reads are retried, not fatal.
func TestRPCBlockFeesWaitForALaggingBackend(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	dynamic := pools[0]
	dynamic.Key, dynamic.TickSpacing = domain.PoolKey{Kind: domain.PoolKindCamelotV3, Address: addressAt(30)}, 60
	pools = append(pools, dynamic)
	blocks := testBlocks(head, 1, pools)
	chain, server := newTestChain(t, 11, blocks...)
	chain.lagging = 3
	run := startFollowerWith(t, config.Config{SearchConfig: "search.json"}, server.URL, head, pools, nil)
	if update := run.next(t); update.block.Ref != blocks[0].ref || update.fees == nil {
		t.Fatal("fees for the RPC block not loaded after the backend caught up", update.block.Ref)
	}
	if chain.calls("eth_call") < 4 {
		t.Fatal("lagging reads were not retried", chain.methods)
	}
}
