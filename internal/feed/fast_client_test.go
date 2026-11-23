package feed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestFastClientAuthReconnectAndInvalidationOrder(t *testing.T) {
	frame := encodeFastFrame(t, fastFrame())
	var connections atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-test-key" {
			t.Error("missing bearer header")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		n := connections.Add(1)
		if err := conn.Write(r.Context(), websocket.MessageBinary, frame); err != nil {
			return
		}
		if n == 1 {
			return
		}
		waitForClose(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	client, err := NewFastClient(feedURL(server), "local-test-key", ClientOptions{ReconnectBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan string, 8)
	stop := errors.New("consumer stopped")
	run := startRun(t, func(ctx context.Context) error {
		seen := 0
		return client.Run(ctx, func(message FastMessage) error {
			seen++
			events <- "message"
			if seen == 2 {
				return stop
			}
			return nil
		}, func() error { events <- "invalidate"; return nil })
	})
	if err := run.wait(t); !errors.Is(err, stop) {
		t.Fatalf("handler error lost: %v", err)
	}
	for i, want := range []string{"invalidate", "message", "invalidate", "message", "invalidate"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("event %d = %s, want %s", i, got, want)
			}
		default:
			t.Fatalf("missing event %d", i)
		}
	}
}

func TestFastClientRejectsCredentialsWithoutEcho(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.Write([]byte(r.Header.Get("Authorization")))
			}))
			t.Cleanup(server.Close)
			client, err := NewFastClient(feedURL(server), "secret-local-key", ClientOptions{})
			if err != nil {
				t.Fatal(err)
			}
			err = client.Run(context.Background(), func(FastMessage) error { t.Error("unexpected message"); return nil }, func() error { return nil })
			var rejection *fastRejected
			if !errors.As(err, &rejection) || rejection.status != status || strings.Contains(err.Error(), "secret-local-key") {
				t.Fatalf("unsafe or missing rejection: %v", err)
			}
		})
	}
	for _, rawURL := range []string{"ws://user:secret@example.test/feed", "ws://example.test/?key=secret", "ws://example.test/#secret"} {
		if _, err := NewFastClient(rawURL, "key", ClientOptions{}); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("credentials URL accepted or echoed: %v", err)
		}
	}
}

func TestFastClientCancellationInvalidates(t *testing.T) {
	frame := encodeFastFrame(t, fastFrame())
	server := newFeedServer(t, nil, func(ctx context.Context, conn *websocket.Conn) {
		conn.Write(ctx, websocket.MessageBinary, frame)
		waitForClose(ctx, conn)
	})
	client, err := NewFastClient(feedURL(server), "local-test-key", ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resets := 0
	err = client.Run(ctx, func(FastMessage) error { cancel(); return nil }, func() error { resets++; return nil })
	if err != nil || resets != 2 {
		t.Fatalf("cancellation error=%v resets=%d", err, resets)
	}
}
