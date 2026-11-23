package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestStrippedFlashblocksRetireBeforeHTTPAndIgnoreDuplicate(t *testing.T) {
	raw, err := os.ReadFile("../feed/testdata/base-flashblocks-index-zero.json")
	if err != nil {
		t.Fatal(err)
	}
	block, err := feed.DecodeFlashblockEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	var heartbeat map[string]any
	if err := json.Unmarshal(raw, &heartbeat); err != nil {
		t.Fatal(err)
	}
	heartbeat["index"] = 1
	delete(heartbeat, "base")
	heartbeat["diff"].(map[string]any)["transactions"] = []any{}
	next, err := json.Marshal(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for _, frame := range [][]byte{raw, raw, next} {
			if err := conn.Write(r.Context(), websocket.MessageText, frame); err != nil {
				return
			}
		}
		conn.Read(r.Context())
	}))
	defer ws.Close()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	httpRPC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requests []struct {
			ID     uint64
			Params []string
		}
		if err := json.NewDecoder(r.Body).Decode(&requests); err != nil {
			t.Error(err)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		results := make([]any, len(requests))
		for i, request := range requests {
			receipt := map[string]any{"transactionHash": request.Params[0], "blockNumber": fmt.Sprintf("0x%x", block.Number), "transactionIndex": fmt.Sprintf("0x%x", i), "status": "0x1", "logs": []any{}}
			results[i] = map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": receipt}
		}
		json.NewEncoder(w).Encode(results)
	}))
	defer httpRPC.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	out := make(chan streamEvent, 8)
	done := make(chan error, 1)
	go func() {
		done <- followFlashblocks(ctx, "ws"+strings.TrimPrefix(ws.URL, "http"), rpc.NewClient(httpRPC.URL, rpc.Options{}), true, out, &atomic.Uint64{}, nil)
	}()
	defer func() { cancel(); <-done }()
	receive := func() streamEvent {
		select {
		case event := <-out:
			return event
		case <-ctx.Done():
			t.Fatal("stream completion timed out")
			return streamEvent{}
		}
	}
	reset := receive()
	if !reset.reset {
		t.Fatal("connection did not reset first")
	}
	close(reset.accepted)
	pending := receive()
	if !pending.flashPending {
		t.Fatal("old prefix was not retired before HTTP")
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("receipt request missing")
	}
	close(release)
	accepted := receive()
	if accepted.flash == nil || accepted.flash.MissingReceipts || accepted.flash.Index != 0 || !accepted.flash.Transactions[0].Successful {
		t.Fatal("incomplete frame escaped completion", accepted)
	}
	// The identical raw index-zero frame must not retire the accepted prefix.
	if event := receive(); !event.flashPending {
		if event.reset {
			close(event.accepted)
		}
		t.Fatal("duplicate changed prefix lifecycle", event)
	}
	if event := receive(); event.flash == nil || event.flash.Index != 1 || event.flash.MissingReceipts {
		t.Fatal("empty heartbeat was not completed", event)
	}
}

func TestUnavailableFlashReceiptsStopAfterThreeRounds(t *testing.T) {
	raw, err := os.ReadFile("../feed/testdata/base-flashblocks-index-zero.json")
	if err != nil {
		t.Fatal(err)
	}
	block, err := feed.DecodeFlashblockEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []struct{ ID uint64 }
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		results := make([]any, len(batch))
		for i, call := range batch {
			calls++
			results[i] = map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": nil}
		}
		json.NewEncoder(w).Encode(results)
	}))
	defer server.Close()
	_, err = completeFlashReceipts(t.Context(), rpc.NewClient(server.URL, rpc.Options{}), block, 0)
	if err == nil || !strings.Contains(err.Error(), "three rounds") {
		t.Fatal(err)
	}
	if calls != 3*len(block.Transactions) {
		t.Fatalf("receipt calls=%d transactions=%d", calls, len(block.Transactions))
	}
}
