package remote

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubmitClientStaysOnOneWarmConnection(t *testing.T) {
	var connections, requests atomic.Int64
	var chain atomic.Value
	chain.Store("0xa")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + chain.Load().(string) + `"}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	if _, err := openSubmitClient(ctx, server.URL, "", 10, 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// Wait on progress, not wall time: a loaded race run can delay ticks.
	for deadline := time.Now().Add(2 * time.Second); requests.Load() < 4 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	cancel()
	time.Sleep(50 * time.Millisecond) // let an in-flight keepalive finish
	seen := requests.Load()
	if seen < 4 || connections.Load() != 1 {
		t.Fatalf("keepalive did not reuse one connection: requests=%d connections=%d", seen, connections.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if requests.Load() != seen {
		t.Fatal("keepalive continued after shutdown")
	}

	chain.Store("0x2105")
	if _, err := openSubmitClient(t.Context(), server.URL, "", 10, time.Hour); err == nil {
		t.Fatal("submission endpoint for another chain accepted")
	}
}

func TestFanoutOpensOnlyPathsOnTheRightChain(t *testing.T) {
	serve := func(chain string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + chain + `"}`))
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	good, wrong := serve("0xa")+"/v2/secret", serve("0x2105")
	paths, report := openFanout(t.Context(), " "+good+" ,,"+wrong, 10)
	if len(paths) != 1 || report == nil || paths[0].Name != "127.0.0.1" {
		t.Fatalf("fan-out paths %+v", paths)
	}
	if none, report := openFanout(t.Context(), "", 10); none != nil || report != nil {
		t.Fatal("empty fan-out list opened paths")
	}
}

// Robinhood's official sequencer serves only eth_sendRawTransaction and
// answers eth_chainId with -32601. It is accepted as a
// submit-only endpoint, and its error replies still keep the one connection
// warm. Signatures bind the chain ID, so a transaction cannot execute on a
// misnamed endpoint's chain.
func TestSubmitClientAcceptsASubmitOnlySequencer(t *testing.T) {
	var connections, requests atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"the method eth_chainId does not exist/is not available"}}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := openSubmitClient(ctx, server.URL, "", 4663, 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); requests.Load() < 4 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if requests.Load() < 4 || connections.Load() != 1 {
		t.Fatalf("submit-only keepalive: requests=%d connections=%d", requests.Load(), connections.Load())
	}
}

func TestPinnedSubmissionEndpoint(t *testing.T) {
	for _, tc := range []struct{ in, url, dial string }{
		{"https://sequencer.example", "https://sequencer.example", ""},
		{"https://sequencer.example#ip=192.0.2.10", "https://sequencer.example", "192.0.2.10:443"},
		{"https://sequencer.example:8443/rpc#ip=2001:db8::5", "https://sequencer.example:8443/rpc", "[2001:db8::5]:8443"},
		{"http://127.0.0.1:8547#ip=127.0.0.2", "http://127.0.0.1:8547", "127.0.0.2:8547"},
	} {
		url, dial, err := pinnedEndpoint(tc.in)
		if err != nil || url != tc.url || dial != tc.dial {
			t.Fatal(tc.in, url, dial, err)
		}
	}
	for _, bad := range []string{"https://sequencer.example#ip=", "https://sequencer.example#host=192.0.2.4", "ftp://sequencer.example#ip=192.0.2.4"} {
		if _, _, err := pinnedEndpoint(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
