package remote

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestRPCReorgToLowerHeadReseedsAtFixedHash(t *testing.T) {
	store, pools := poolFixture(t)
	old := store.Block()
	old.Number = 11
	replacement := store.Block()
	replacement.Hash = domain.Hash{99}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch call.Method {
		case "eth_getBlockByNumber":
			var tag string
			json.Unmarshal(call.Params[0], &tag)
			if tag == "0xb" {
				t.Error("requested detached higher head before observing shorter canonical chain")
			}
			result = map[string]any{"number": "0xa", "hash": replacement.Hash.Hex(), "parentHash": replacement.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", replacement.Timestamp), "l1BlockNumber": "0x64", "baseFeePerGas": "0x1", "gasLimit": "0x100000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		case "eth_call":
			var position struct {
				BlockHash        string
				RequireCanonical bool
			}
			if err := json.Unmarshal(call.Params[1], &position); err != nil || position.BlockHash != replacement.Hash.Hex() || !position.RequireCanonical {
				t.Error("bootstrap did not use fixed canonical hash", err)
			}
			calls.Add(1)
			result = "0x" + hex.EncodeToString(wordData(1_000_000, 4_000_000, 0))
		default:
			t.Error("unexpected method", call.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer server.Close()
	cfg, err := (config.Config{RPCURL: server.URL, RPCPoll: time.Millisecond}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	out := make(chan canonicalUpdate)
	done := make(chan error, 1)
	go func() {
		done <- followRPC(ctx, cfg, rpc.NewClient(server.URL, rpc.Options{}), specsFor(pools), old, 1, nil, nil, out)
	}()
	select {
	case update := <-out:
		if update.reset == nil || update.block.Ref != replacement || calls.Load() != 2 {
			t.Fatal("reorg failed to reseed complete pool catalog")
		}
		p, _ := update.reset.Pool(pools[0].Key)
		if p.Reserve1.Int64() != 4_000_000 {
			t.Fatal("reorg reused detached state")
		}
		cancel()
		close(update.accepted)
	case err := <-done:
		t.Fatal("source stopped before recovery", err)
	case <-ctx.Done():
		t.Fatal("reorg recovery stalled")
	}
	<-done
}

func TestCatchupReorgRefreshesTipBeforeReseeding(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	var fork atomic.Bool
	makeRef := func(number uint64) domain.BlockRef {
		ref := head
		ref.Number = number
		ref.Hash = domain.Hash{byte(number)}
		ref.ParentHash = domain.Hash{byte(number - 1)}
		if fork.Load() && number >= 12 {
			ref.Hash = domain.Hash{byte(number + 100)}
			ref.ParentHash = domain.Hash{byte(number + 99)}
		}
		return ref
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&call)
		var result any
		if call.Method == "eth_call" {
			result = "0x" + hex.EncodeToString(wordData(1_000_000, 4_000_000, 0))
		} else {
			var tag string
			json.Unmarshal(call.Params[0], &tag)
			number := uint64(13)
			if fork.Load() {
				number = 14
			}
			if tag != "latest" {
				var err error
				number, err = strconv.ParseUint(tag[2:], 16, 64)
				if err != nil {
					t.Error(err)
				}
			}
			ref := makeRef(number)
			result = map[string]any{"number": fmt.Sprintf("0x%x", number), "hash": ref.Hash.Hex(), "parentHash": ref.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", ref.Timestamp), "l1BlockNumber": "0x64", "baseFeePerGas": "0x1", "gasLimit": "0x10000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer server.Close()
	cfg, err := (config.Config{RPCURL: server.URL, RPCPoll: time.Millisecond}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	updates := make(chan canonicalUpdate)
	done := make(chan error, 1)
	go func() {
		done <- followRPC(ctx, cfg, rpc.NewClient(server.URL, rpc.Options{}), specsFor(pools), head, 1, nil, nil, updates)
	}()
	select {
	case update := <-updates:
		if update.block.Ref.Number != 11 || !update.catchingUp || update.nearTip {
			t.Fatal("first catchup block missing")
		}
		fork.Store(true)
		close(update.accepted)
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("first block timed out")
	}
	select {
	case update := <-updates:
		if update.reset == nil || update.block.Ref != makeRef(14) {
			t.Fatal("did not recover at fresh canonical tip", update.block.Ref)
		}
		cancel()
		close(update.accepted)
	case err := <-done:
		t.Fatal("reseed used an obsolete tip", err)
	case <-ctx.Done():
		t.Fatal("reorg recovery timed out")
	}
	<-done
}

// chainHeaders serves headers of a linear chain whose block n has hash {n},
// counting reads per tag. Hashes are read through hashed: false hides a block.
func chainHeaders(t *testing.T, latest func() uint64, hashed func(uint64) bool) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	reads := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&call)
		var tag string
		json.Unmarshal(call.Params[0], &tag)
		mu.Lock()
		reads[tag]++
		mu.Unlock()
		var number uint64
		switch {
		case call.Method == "eth_getBlockByHash":
			hash, err := domain.ParseHash(tag)
			number = uint64(hash[0])
			if err != nil || hash != (domain.Hash{byte(number)}) || !hashed(number) {
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": nil})
				return
			}
		case tag == "latest":
			number = latest()
		default:
			number, _ = strconv.ParseUint(tag[2:], 16, 64)
		}
		result := map[string]any{"number": fmt.Sprintf("0x%x", number), "hash": domain.Hash{byte(number)}.Hex(), "parentHash": domain.Hash{byte(number - 1)}.Hex(), "timestamp": "0x1", "l1BlockNumber": "0x64", "baseFeePerGas": "0x1", "gasLimit": "0x10000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return server, func(tag string) int {
		mu.Lock()
		defer mu.Unlock()
		return reads[tag]
	}
}

// Catch-up reads each header once: the next block's parent hash proves it,
// so only the tip is read again after its receipts.
func TestCatchupReadsEachHeaderOnce(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	tip := head.Number + 5
	server, reads := chainHeaders(t, func() uint64 { return tip }, func(uint64) bool { return true })
	cfg, err := (config.Config{RPCURL: server.URL, RPCPoll: 10 * time.Second}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	updates := make(chan canonicalUpdate)
	done := make(chan error, 1)
	go func() {
		done <- followRPC(ctx, cfg, rpc.NewClient(server.URL, rpc.Options{}), specsFor(pools), head, 1, nil, nil, updates)
	}()
	for number := head.Number + 1; number <= tip; number++ {
		select {
		case update := <-updates:
			if update.reset != nil || update.block.Ref.Number != number {
				t.Fatal("unexpected update at", number, update.block.Ref.Number)
			}
			close(update.accepted)
		case err := <-done:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("catch-up stalled at", number)
		}
	}
	for number := head.Number + 1; number < tip; number++ {
		if n := reads(fmt.Sprintf("0x%x", number)); n != 1 {
			t.Errorf("block %d header read %d times", number, n)
		}
	}
	// The tip comes from latest, which is read again once after its receipts.
	if n, m := reads(fmt.Sprintf("0x%x", tip)), reads("latest"); n != 0 || m != 2 {
		t.Errorf("tip read %d times by number and latest %d times", n, m)
	}
	cancel()
	<-done
}

// A feed wake names a block the RPC node may not serve yet. The follower
// polls for that hash and picks the block up before the next wake, without
// polling latest.
func TestFeedWakeFollowsTheBlockBeforeTheNextWake(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	var ready atomic.Bool
	server, reads := chainHeaders(t, func() uint64 {
		if ready.Load() {
			return head.Number + 1
		}
		return head.Number
	}, func(uint64) bool { return ready.Load() })
	cfg, err := (config.Config{RPCURL: server.URL, RPCPoll: 10 * time.Second, RPCWait: 30 * time.Second}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	wake := make(chan domain.Hash, 1)
	updates := make(chan canonicalUpdate)
	done := make(chan error, 1)
	go func() {
		done <- followRPC(ctx, cfg, rpc.NewClient(server.URL, rpc.Options{}), specsFor(pools), head, 1, wake, nil, updates)
	}()
	time.Sleep(20 * time.Millisecond) // the follower is idle at the head
	latestBefore := reads("latest")
	offerWake(wake, domain.Hash{byte(head.Number + 1)})
	time.Sleep(30 * time.Millisecond) // the node serves the block after the wake
	ready.Store(true)
	woke := time.Now()
	select {
	case update := <-updates:
		if update.block.Ref.Number != head.Number+1 {
			t.Fatal("unexpected block", update.block.Ref.Number)
		}
		if late := time.Since(woke); late > 200*time.Millisecond {
			t.Fatal("block followed too late", late)
		}
		// Only the recheck after the tip's receipts reads latest.
		if n := reads("latest") - latestBefore; n != 1 {
			t.Fatal("latest read after the wake", n)
		}
		cancel()
		close(update.accepted)
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("block not followed without a second wake")
	}
	<-done
}

// Far behind the tip, reading every pool once is cheaper than two reads a
// block, so the follower reseeds at the tip.
func TestFarBehindTheTipReseedsInsteadOfCatchingUp(t *testing.T) {
	store, pools := poolFixture(t)
	head := store.Block()
	cfg, err := (config.Config{RPCPoll: 10 * time.Second}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	tip := head.Number + cfg.Network.Blocks(catchUpLimit) + 1
	var headers atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&call)
		var result any
		if call.Method == "eth_call" {
			result = "0x" + hex.EncodeToString(wordData(1_000_000, 4_000_000, 0))
		} else {
			headers.Add(1)
			var tag string
			json.Unmarshal(call.Params[0], &tag)
			if tag != "latest" && tag != fmt.Sprintf("0x%x", tip) {
				t.Error("caught up through", tag)
			}
			result = map[string]any{"number": fmt.Sprintf("0x%x", tip), "hash": domain.Hash{0xaa}.Hex(), "parentHash": domain.Hash{0xab}.Hex(), "timestamp": "0x1", "l1BlockNumber": "0x64", "baseFeePerGas": "0x1", "gasLimit": "0x10000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer server.Close()
	cfg.RPCURL = server.URL
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	updates := make(chan canonicalUpdate)
	done := make(chan error, 1)
	go func() {
		done <- followRPC(ctx, cfg, rpc.NewClient(server.URL, rpc.Options{}), specsFor(pools), head, 1, nil, nil, updates)
	}()
	select {
	case update := <-updates:
		if update.reset == nil || update.block.Ref.Number != tip {
			t.Fatal("did not reseed at the tip", update.block.Ref.Number)
		}
		cancel()
		close(update.accepted)
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("no reseed")
	}
	<-done
}
