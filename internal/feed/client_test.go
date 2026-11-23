package feed

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newFeedServer accepts WebSocket connections and calls handler for each one.
// The connection is closed after handler returns.
func newFeedServer(t *testing.T, opts *websocket.AcceptOptions, handler func(ctx context.Context, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, opts)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		handler(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	return server
}

func feedURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// frameForSequence builds a valid single-message frame.
func frameForSequence(sequence uint64) string {
	return testFrame(testMessage(sequence, testHashA, `{"kind":3,"blockNumber":519141801,"timestamp":1761744000}`, ""))
}

// waitForClose reads until the peer closes the connection.
func waitForClose(ctx context.Context, conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

type runHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// startRun runs fn in the background and guarantees it stops before the test
// ends, so a leaked goroutine cannot outlive its server.
func startRun(t *testing.T, fn func(ctx context.Context) error) *runHandle {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	handle := &runHandle{cancel: cancel, done: make(chan struct{})}
	go func() {
		handle.err = fn(ctx)
		close(handle.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-handle.done:
		case <-time.After(5 * time.Second):
			t.Errorf("run did not stop after context cancellation")
		}
	})
	return handle
}

// wait blocks until the background run returns.
func (h *runHandle) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-h.done:
		return h.err
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return within 5s")
		return nil
	}
}

func receiveMessages(t *testing.T, messages <-chan Message, count int) []Message {
	t.Helper()
	received := make([]Message, 0, count)
	for len(received) < count {
		select {
		case message := <-messages:
			received = append(received, message)
		case <-time.After(5 * time.Second):
			t.Fatalf("received %d of %d messages", len(received), count)
		}
	}
	return received
}

func TestClientDefaults(t *testing.T) {
	client, err := NewClient("ws://127.0.0.1:8547", ClientOptions{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.reconnectBackoff != 200*time.Millisecond {
		t.Fatalf("reconnectBackoff = %s, want 200ms", client.reconnectBackoff)
	}
	if client.maxBackoff != 5*time.Second {
		t.Fatalf("maxBackoff = %s, want 5s", client.maxBackoff)
	}
	if client.readLimit != 16<<20 {
		t.Fatalf("readLimit = %d, want %d", client.readLimit, 16<<20)
	}
}

func TestNewClientRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		url  string
		opts ClientOptions
	}{
		{name: "empty url", url: ""},
		{name: "http scheme", url: "http://127.0.0.1:8547/feed"},
		{name: "missing host", url: "ws:///feed"},
		{name: "unparsable url", url: "ws://[::1"},
		{name: "negative backoff", url: "ws://127.0.0.1:8547", opts: ClientOptions{ReconnectBackoff: -time.Second}},
		{name: "negative max backoff", url: "ws://127.0.0.1:8547", opts: ClientOptions{MaxBackoff: -time.Second}},
		{name: "negative read limit", url: "ws://127.0.0.1:8547", opts: ClientOptions{ReadLimit: -1}},
		{
			name: "max backoff below reconnect backoff",
			url:  "ws://127.0.0.1:8547",
			opts: ClientOptions{ReconnectBackoff: time.Second, MaxBackoff: time.Millisecond},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewClient(test.url, test.opts); err == nil {
				t.Fatalf("NewClient(%q) succeeded, want error", test.url)
			}
		})
	}
}

func TestClientDeliversFrame(t *testing.T) {
	const sequence = uint64(496933984)
	tests := []struct {
		name string
		kind websocket.MessageType
	}{
		{name: "text", kind: websocket.MessageText},
		{name: "binary", kind: websocket.MessageBinary},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newFeedServer(t, nil, func(ctx context.Context, conn *websocket.Conn) {
				if err := conn.Write(ctx, test.kind, []byte(frameForSequence(sequence))); err != nil {
					return
				}
				waitForClose(ctx, conn)
			})
			client, err := NewClient(feedURL(server), ClientOptions{})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}

			messages := make(chan Message, 1)
			run := startRun(t, func(ctx context.Context) error {
				return client.Run(ctx, func(message Message) error {
					messages <- message
					return nil
				})
			})

			received := receiveMessages(t, messages, 1)[0]
			if received.Sequence != sequence {
				t.Fatalf("Sequence = %d, want %d", received.Sequence, sequence)
			}
			if received.BlockHash.Hex() != testHashA {
				t.Fatalf("BlockHash = %s, want %s", received.BlockHash.Hex(), testHashA)
			}
			if received.Header.Kind != 3 || received.Header.L1BlockNumber != 519141801 {
				t.Fatalf("Header = %+v, want kind 3 and L1 block 519141801", received.Header)
			}

			run.cancel()
			if err := run.wait(t); err != nil {
				t.Fatalf("Run returned %v after cancellation, want nil", err)
			}
		})
	}
}

// recordingWriter keeps the handshake response headers readable after Accept
// hijacks the connection.
type recordingWriter struct {
	http.ResponseWriter
	header http.Header
}

func (w *recordingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

func TestClientDisablesCompression(t *testing.T) {
	type handshake struct {
		offered  string
		accepted string
	}
	handshakes := make(chan handshake, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &recordingWriter{ResponseWriter: w, header: w.Header()}
		// The server offers compression, so a client that enabled it would
		// negotiate permessage-deflate.
		conn, err := websocket.Accept(recorder, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		handshakes <- handshake{
			offered:  r.Header.Get("Sec-WebSocket-Extensions"),
			accepted: recorder.header.Get("Sec-WebSocket-Extensions"),
		}
		waitForClose(r.Context(), conn)
	}))
	t.Cleanup(server.Close)

	client, err := NewClient(feedURL(server), ClientOptions{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	run := startRun(t, func(ctx context.Context) error {
		return client.Run(ctx, func(Message) error { return nil })
	})

	var result handshake
	select {
	case result = <-handshakes:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept a connection")
	}
	if result.offered != "" {
		t.Fatalf("client offered %q, want no compression offer", result.offered)
	}
	if result.accepted != "" {
		t.Fatalf("server negotiated %q, want no compression", result.accepted)
	}

	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("Run returned %v after cancellation, want nil", err)
	}
}

func TestClientCancellationStopsRun(t *testing.T) {
	accepted := make(chan struct{}, 1)
	server := newFeedServer(t, nil, func(ctx context.Context, conn *websocket.Conn) {
		accepted <- struct{}{}
		waitForClose(ctx, conn)
	})
	client, err := NewClient(feedURL(server), ClientOptions{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	run := startRun(t, func(ctx context.Context) error {
		return client.Run(ctx, func(Message) error { return nil })
	})
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept a connection")
	}

	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("Run returned %v after cancellation, want nil", err)
	}
}

func TestClientHandlerErrorStopsRun(t *testing.T) {
	const sequence = uint64(8388608)
	sentinel := errors.New("handler gave up")

	server := newFeedServer(t, nil, func(ctx context.Context, conn *websocket.Conn) {
		if err := conn.Write(ctx, websocket.MessageText, []byte(frameForSequence(sequence))); err != nil {
			return
		}
		waitForClose(ctx, conn)
	})
	client, err := NewClient(feedURL(server), ClientOptions{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	run := startRun(t, func(ctx context.Context) error {
		return client.Run(ctx, func(Message) error { return sentinel })
	})
	runErr := run.wait(t)
	if runErr == nil {
		t.Fatal("Run = nil, want handler error")
	}
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("Run error = %v, want it to wrap the handler error", runErr)
	}
	if !strings.Contains(runErr.Error(), fmt.Sprint(sequence)) {
		t.Fatalf("Run error = %v, want it to contain sequence %d", runErr, sequence)
	}
}

func TestClientReconnectsAfterDrop(t *testing.T) {
	var accepts atomic.Int32
	server := newFeedServer(t, nil, func(ctx context.Context, conn *websocket.Conn) {
		attempt := accepts.Add(1)
		if err := conn.Write(ctx, websocket.MessageText, []byte(frameForSequence(uint64(attempt)))); err != nil {
			return
		}
		if attempt == 1 {
			_ = conn.Close(websocket.StatusNormalClosure, "drop")
			return
		}
		waitForClose(ctx, conn)
	})
	client, err := NewClient(feedURL(server), ClientOptions{ReconnectBackoff: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	messages := make(chan Message, 2)
	run := startRun(t, func(ctx context.Context) error {
		return client.Run(ctx, func(message Message) error {
			messages <- message
			return nil
		})
	})

	received := receiveMessages(t, messages, 2)
	if received[0].Sequence != 1 || received[1].Sequence != 2 {
		t.Fatalf("sequences = %d, %d, want 1, 2", received[0].Sequence, received[1].Sequence)
	}
	if got := accepts.Load(); got != 2 {
		t.Fatalf("server accepted %d connections, want 2", got)
	}

	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("Run returned %v after cancellation, want nil", err)
	}
}

// Robinhood's relay answers 400 "Compression is required" to a handshake
// without permessage-deflate. With Compress the client offers it, and a
// frame the server compresses still decodes.
func TestClientOffersCompressionWhenAsked(t *testing.T) {
	const sequence = uint64(496933984)
	offered := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate") {
			http.Error(w, "Compression is required: offer permessage-deflate (or Arbitrum-permessage-deflate).", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover, CompressionThreshold: 1})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		offered <- r.Header.Get("Sec-WebSocket-Extensions")
		if err := conn.Write(r.Context(), websocket.MessageText, []byte(frameForSequence(sequence))); err != nil {
			return
		}
		waitForClose(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(feedURL(server), ClientOptions{Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	messages := make(chan Message, 1)
	run := startRun(t, func(ctx context.Context) error {
		return client.Run(ctx, func(message Message) error {
			messages <- message
			return nil
		})
	})
	if received := receiveMessages(t, messages, 1)[0]; received.Sequence != sequence {
		t.Fatalf("Sequence = %d, want %d", received.Sequence, sequence)
	}
	<-offered
	run.cancel()
	if err := run.wait(t); err != nil {
		t.Fatalf("Run returned %v after cancellation, want nil", err)
	}
}
