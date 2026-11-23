package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestIdleFollowerDoesNotRereadTheSameHeadByHeight(t *testing.T) {
	store, _ := poolFixture(t)
	head := store.Block()
	var duplicate, latest atomic.Int32
	observed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64
			Params []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&call)
		var tag string
		json.Unmarshal(call.Params[0], &tag)
		if tag == "latest" {
			if latest.Add(1) == 3 {
				close(observed)
			}
		} else {
			duplicate.Add(1)
		}
		result := map[string]any{"number": fmt.Sprintf("0x%x", head.Number), "hash": head.Hash.Hex(), "parentHash": head.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", head.Timestamp), "baseFeePerGas": "0x1", "gasLimit": "0x10000", "miner": "0x0000000000000000000000000000000000000000", "transactions": []string{}}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- followRPC(ctx, config.Config{RPCPoll: time.Millisecond}, rpc.NewClient(server.URL, rpc.Options{}), nil, head, 1, nil, nil, make(chan canonicalUpdate))
	}()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("poll stalled")
	}
	cancel()
	<-done
	if duplicate.Load() != 0 {
		t.Fatalf("idle head required %d redundant height reads", duplicate.Load())
	}
}

func TestStaleHashHintCannotRewindAcceptedState(t *testing.T) {
	store, _ := poolFixture(t)
	head := store.Block()
	old := head
	old.Number--
	old.Hash = domain.Hash{9}
	old.ParentHash = domain.Hash{8}
	var latest atomic.Int32
	checked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&call)
		ref := head
		if call.Method == "eth_getBlockByHash" {
			ref = old
		} else {
			var tag string
			json.Unmarshal(call.Params[0], &tag)
			if tag == "latest" && latest.Add(1) == 2 {
				close(checked)
			}
		}
		result := map[string]any{"number": fmt.Sprintf("0x%x", ref.Number), "hash": ref.Hash.Hex(), "parentHash": ref.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", ref.Timestamp), "baseFeePerGas": "0x1", "gasLimit": "0x10000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	hints := make(chan domain.Hash, 1)
	hints <- old.Hash
	out := make(chan canonicalUpdate)
	done := make(chan error, 1)
	go func() {
		done <- followRPC(ctx, config.Config{RPCPoll: time.Hour}, rpc.NewClient(server.URL, rpc.Options{}), nil, head, 1, hints, nil, out)
	}()
	select {
	case <-checked:
	case update := <-out:
		t.Fatal("stale hint rewound state", update.block.Ref)
	case err := <-done:
		t.Fatal("stale hint stopped the follower", err)
	case <-ctx.Done():
		t.Fatal("stale hint was not checked")
	}
	cancel()
	<-done
}

func TestMovingTipDoesNotPublishRememberedHeadAsLive(t *testing.T) {
	for _, lag := range []uint64{1, 2} {
		t.Run(fmt.Sprintf("lag_%d", lag), func(t *testing.T) {
			store, _ := poolFixture(t)
			head := store.Block()
			var latest atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call struct {
					ID     uint64
					Method string
					Params []json.RawMessage
				}
				json.NewDecoder(r.Body).Decode(&call)
				if call.Method == "eth_getBlockReceipts" {
					json.NewEncoder(w).Encode(map[string]any{"id": call.ID, "result": []any{}})
					return
				}
				var tag string
				json.Unmarshal(call.Params[0], &tag)
				number := uint64(11)
				if tag == "latest" && latest.Add(1) > 1 {
					number = 11 + lag
				}
				ref := head
				ref.Number = number
				ref.Hash = domain.Hash{byte(number)}
				ref.ParentHash = domain.Hash{byte(number - 1)}
				result := map[string]any{"number": fmt.Sprintf("0x%x", number), "hash": ref.Hash.Hex(), "parentHash": ref.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", ref.Timestamp), "baseFeePerGas": "0x1", "gasLimit": "0x10000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
				json.NewEncoder(w).Encode(map[string]any{"id": call.ID, "result": result})
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			out := make(chan canonicalUpdate)
			done := make(chan error, 1)
			go func() {
				done <- followRPC(ctx, config.Config{RPCPoll: time.Millisecond}, rpc.NewClient(server.URL, rpc.Options{}), nil, head, 1, nil, nil, out)
			}()
			select {
			case update := <-out:
				cancel()
				close(update.accepted)
				<-done
				if !update.catchingUp {
					t.Fatal("a remembered tip was published as live after canonical head advanced")
				}
				if update.nearTip != (lag == 1) {
					t.Fatal("maintenance lag classification differs", update.nearTip, lag)
				}
			case err := <-done:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal("follower stalled")
			}
		})
	}
}
