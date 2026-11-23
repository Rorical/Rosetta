package feed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestStandardClientRequestsContiguousResumeAfterReconnect(t *testing.T) {
	requests := make(chan string, 2)
	var connections atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Arbitrum-Requested-Sequence-Number")
		if r.Header.Get("Arbitrum-Feed-Client-Version") != "2" {
			t.Error("missing pinned client handshake version")
		}
		w.Header().Set("Arbitrum-Feed-Server-Version", "2")
		w.Header().Set("Arbitrum-Chain-Id", "42161")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if connections.Add(1) == 1 {
			conn.Write(r.Context(), websocket.MessageText, []byte(testFrame(testMessage(21, testHashA, `{}`, ""), testMessage(22, testHashB, `{}`, ""))))
			return
		}
		conn.Write(r.Context(), websocket.MessageText, []byte(frameForSequence(23)))
		waitForClose(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	next := uint64(21)
	client, err := NewClient(feedURL(server), ClientOptions{NextSequence: &next, RequireHeaders: true, ReconnectBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("finished")
	run := startRun(t, func(ctx context.Context) error {
		return client.Run(ctx, func(m Message) error {
			if m.Sequence == 23 {
				return stop
			}
			return nil
		})
	})
	if err := run.wait(t); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	for _, want := range []string{"21", "23"} {
		if got := <-requests; got != want {
			t.Fatalf("resume request=%s want=%s", got, want)
		}
	}
}

func TestStandardClientRejectsIncompatibleHandshake(t *testing.T) {
	for _, test := range []struct{ name, version, chain string }{
		{"wrong chain", "2", "1"}, {"wrong version", "9", "42161"}, {"missing headers", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Arbitrum-Feed-Server-Version", test.version)
				w.Header().Set("Arbitrum-Chain-Id", test.chain)
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				waitForClose(r.Context(), conn)
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(feedURL(server), ClientOptions{RequireHeaders: true})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = client.Run(ctx, func(Message) error { t.Error("incompatible server delivered data"); return nil })
			var failure *protocolError
			if !errors.As(err, &failure) {
				t.Fatalf("missing protocol rejection: %v", err)
			}
		})
	}
}
