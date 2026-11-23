package remote

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func poolFixture(t *testing.T) (*poolstate.Store, []pool.State) {
	t.Helper()
	pools := []pool.State{
		{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: addressAt(10)}, Token0: addressAt(1), Token1: addressAt(2), Reserve0: big.NewInt(1_000_000), Reserve1: big.NewInt(2_000_000), Fee: 3000},
		{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: addressAt(20)}, Token0: addressAt(1), Token1: addressAt(2), Reserve0: big.NewInt(1_000_000), Reserve1: big.NewInt(1_000_000), Fee: 3000},
	}
	store := poolstate.New()
	for _, p := range pools {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.BeginBlock(domain.BlockRef{Number: 10, Hash: domain.Hash{10}, ParentHash: domain.Hash{9}, Timestamp: uint64(time.Now().Unix()), L1Block: 100}); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	store.ClearDirty()
	return store, pools
}

func wordData(values ...int64) []byte {
	out := make([]byte, len(values)*32)
	for i, value := range values {
		big.NewInt(value).FillBytes(out[i*32 : (i+1)*32])
	}
	return out
}
func syncEvent(key domain.PoolKey, reserve1 int64) feed.FastLog {
	return feed.FastLog{Address: key.Address, Topics: []domain.Hash{uniswapv2.TopicSync}, Data: wordData(1_000_000, reserve1)}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// canonicalServer serves the fixture's parent and one child block whose
// receipt moves the first pool's reserves to 2,100,000.
func canonicalServer(t *testing.T, parent domain.BlockRef, pools []pool.State) (*httptest.Server, domain.BlockRef) {
	t.Helper()
	next := parent
	next.Number++
	next.ParentHash = parent.Hash
	next.Hash = domain.Hash{11}
	tx := domain.Hash{33}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		json.NewDecoder(request.Body).Decode(&call)
		var result any
		switch call.Method {
		case "eth_chainId":
			result = "0xa4b1"
		case "eth_getBlockByHash", "eth_getBlockByNumber":
			var tag string
			json.Unmarshal(call.Params[0], &tag)
			ref := parent
			transactions := []string{}
			if tag == "latest" || tag == "0xb" || tag == next.Hash.Hex() {
				ref = next
				transactions = []string{tx.Hex()}
			}
			result = map[string]any{"number": fmt.Sprintf("0x%x", ref.Number), "hash": ref.Hash.Hex(), "parentHash": ref.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", ref.Timestamp), "l1BlockNumber": "0x64", "baseFeePerGas": "0x1", "gasLimit": "0x1e8480", "miner": domain.Address{}.Hex(), "transactions": transactions}
		case "eth_getBlockReceipts":
			result = []any{map[string]any{"transactionHash": tx.Hex(), "transactionIndex": "0x0", "blockNumber": "0xb", "blockHash": next.Hash.Hex(), "status": "0x1", "logs": []any{map[string]any{"address": pools[0].Key.Address.Hex(), "logIndex": "0x0", "topics": []string{uniswapv2.TopicSync.Hex()}, "data": "0x" + hex.EncodeToString(wordData(1_000_000, 2_100_000))}}}}
		default:
			t.Errorf("unexpected RPC %s", call.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return server, next
}

func TestRPCBackendCanonicalLogsSearchAndSnapshot(t *testing.T) {
	store, pools := poolFixture(t)
	server, next := canonicalServer(t, store.Block(), pools)
	directory := t.TempDir()
	snapshotPath := filepath.Join(directory, "initial.json")
	data, _ := store.EncodeSnapshot()
	os.WriteFile(snapshotPath, data, 0o600)
	searchPath := filepath.Join(directory, "search.json")
	writeJSON(t, searchPath, map[string]any{"k": 1, "max_depth": 2, "max_cycles": 100, "funding": []any{map[string]any{"token": addressAt(1).Hex(), "min_input": "1", "seed_input": "1000", "max_input": "100000"}}})
	cfg, err := config.Parse([]string{"-rpc", server.URL, "-pool-snapshot", snapshotPath, "-search-config", searchPath, "-candidate-out", filepath.Join(directory, "candidates.jsonl"), "-pool-snapshot-out", filepath.Join(directory, "final.json"), "-blocks", "1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	controlDir, err := os.MkdirTemp("/tmp", "rctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(controlDir) })
	cfg.ControlDir = controlDir
	backend, err := (Factory{}).Create(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := backend.Run(ctx); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(cfg.PoolSnapshotOut)
	if err != nil {
		t.Fatal(err)
	}
	final := poolstate.New()
	if err := final.LoadSnapshot(encoded); err != nil {
		t.Fatal(err)
	}
	p, _ := final.Pool(pools[0].Key)
	if final.Block() != next || p.Reserve1.Int64() != 2_100_000 {
		t.Fatal("canonical pool state not advanced")
	}
	if _, err := os.Stat(filepath.Join(controlDir, "control.lock")); err != nil {
		t.Fatalf("run did not open its control directory: %v", err)
	}
	candidates, err := os.ReadFile(cfg.CandidateOut)
	if err != nil || !strings.Contains(string(candidates), `"status":"candidate"`) {
		t.Fatal("shared dirty search did not run", string(candidates), err)
	}
}

func TestFlashblocksPrefixIsolationGapAndCanonicalReset(t *testing.T) {
	store, pools := poolFixture(t)
	branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := &runtime{store: store, fast: branch}
	block := feed.Flashblock{PayloadID: "0x0102030405060708", Number: 11, Base: &feed.FlashblockBase{Number: 11, Parent: store.Block().Hash}, Transactions: []feed.FlashTransaction{{Hash: domain.Hash{1}, Raw: []byte{1}, Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3_000_000)}}}}
	if err := r.acceptFlashblock(t.Context(), block, time.Time{}); err != nil {
		t.Fatal(err)
	}
	version := r.tentative
	view, _, ok := r.flashLogs.View()
	if !ok || !r.Current(version) {
		t.Fatal("tentative view missing")
	}
	p, _ := view.Pool(pools[0].Key)
	original, _ := store.Pool(pools[0].Key)
	if p.Reserve1.Int64() != 3_000_000 || original.Reserve1.Int64() != 2_000_000 {
		t.Fatal("temporary logs changed canonical state")
	}
	if err := r.acceptFlashblock(t.Context(), block, time.Time{}); err != nil || r.tentative != version {
		t.Fatal("identical duplicate changed version", err)
	}
	heartbeat := feed.Flashblock{PayloadID: block.PayloadID, Number: 11, Index: 1}
	if err := r.acceptFlashblock(t.Context(), heartbeat, time.Time{}); err != nil || r.Current(version) {
		t.Fatal("empty subblock failed to retire earlier version", err)
	}
	gap := heartbeat
	gap.Index = 3
	if err := r.acceptFlashblock(t.Context(), gap, time.Time{}); err != nil || r.tentative != (domain.StateVersion{}) {
		t.Fatal("gap retained tentative state", err)
	}
	if _, _, ok := r.flashLogs.View(); ok {
		t.Fatal("gap kept partial branch")
	}
	block.Base.Parent = domain.Hash{99}
	r.acceptFlashblock(t.Context(), block, time.Time{})
	if _, _, ok := r.flashLogs.View(); ok {
		t.Fatal("stream from another parent was accepted")
	}
}

func TestFlashblocksWaitForRPCParentAndDropOnReset(t *testing.T) {
	store, pools := poolFixture(t)
	branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := &runtime{store: store, fast: branch}
	parent := store.Block()
	parent.Number++
	parent.ParentHash = store.Block().Hash
	parent.Hash = domain.Hash{11}
	pending := feed.Flashblock{PayloadID: "0x0102030405060708", Number: 12, Base: &feed.FlashblockBase{Number: 12, Parent: parent.Hash}, Transactions: []feed.FlashTransaction{{Hash: domain.Hash{40}, Raw: []byte{1}, Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3_000_000)}}}}
	if err := r.acceptFlashblock(t.Context(), pending, time.Time{}); err != nil {
		t.Fatal(err)
	}
	suffix := feed.Flashblock{PayloadID: pending.PayloadID, Number: 12, Index: 1, Transactions: []feed.FlashTransaction{{Hash: domain.Hash{41}, Raw: []byte{2}, Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3_100_000)}}}}
	if err := r.acceptFlashblock(t.Context(), suffix, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(r.flashWaiting) != 2 || r.tentative != (domain.StateVersion{}) {
		t.Fatal("unverified parent became current")
	}
	if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: parent}}); err != nil {
		t.Fatal(err)
	}
	view, version, ok := r.flashLogs.View()
	if !ok || version.Anchor != parent || len(r.flashWaiting) != 0 {
		t.Fatal("RPC parent did not attach complete cached prefix")
	}
	p, _ := view.Pool(pools[0].Key)
	if p.Reserve1.Int64() != 3_100_000 {
		t.Fatal("cached suffix was not replayed")
	}
	original, _ := r.store.Pool(pools[0].Key)
	if original.Reserve1.Int64() != 2_000_000 {
		t.Fatal("cached logs leaked into accepted state")
	}
	if err := r.resetTentative(); err != nil {
		t.Fatal(err)
	}
	if err := r.acceptFlashblock(t.Context(), suffix, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if r.tentative != (domain.StateVersion{}) {
		t.Fatal("disconnect permitted mid-prefix resumption")
	}
}

func TestFastRPCReconcilePreservesFutureAndRejectsConflictingReceipt(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			store, pools := poolFixture(t)
			branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
			if err != nil {
				t.Fatal(err)
			}
			r := &runtime{store: store, fast: branch, cfg: config.Config{FastURL: "enabled"}}
			first := feed.FastMessage{BlockNumber: 11, TxHash: domain.Hash{40}, RawTx: []byte{1}, Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 2_500_000)}}}
			future := feed.FastMessage{BlockNumber: 12, TxHash: domain.Hash{41}, RawTx: []byte{2}, Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{syncEvent(pools[0].Key, 3_000_000)}}}
			for _, message := range []feed.FastMessage{first, future} {
				if _, err := branch.Apply(t.Context(), message); err != nil {
					t.Fatal(err)
				}
			}
			next := store.Block()
			next.Number++
			next.ParentHash = next.Hash
			next.Hash = domain.Hash{11}
			log := first.Receipt.Logs[0]
			if conflict {
				log = syncEvent(pools[0].Key, 2_600_000)
			}
			tx := ingest.Transaction{Hash: first.TxHash, Successful: true, Logs: []ingest.Log{{Address: log.Address, Topics: log.Topics, Data: log.Data}}}
			if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: next, Transactions: []domain.Hash{first.TxHash}}, transactions: []ingest.Transaction{tx}}); err != nil {
				t.Fatal(err)
			}
			view, version, _, ok := branch.View()
			if conflict {
				if ok || r.tentative != (domain.StateVersion{}) {
					t.Fatal("conflicting future prefix retained")
				}
				return
			}
			if !ok || version.Anchor != next || !r.Current(version) {
				t.Fatal("valid future prefix discarded")
			}
			p, _ := view.Pool(pools[0].Key)
			accepted, _ := r.store.Pool(pools[0].Key)
			if p.Reserve1.Int64() != 3_000_000 || accepted.Reserve1.Int64() != 2_500_000 {
				t.Fatal("reconcile mixed temporary and accepted state")
			}
		})
	}
}
