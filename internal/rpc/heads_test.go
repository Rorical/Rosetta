package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Heads arrive in order; a notification for another subscription is ignored,
// and a dropped connection is redialed.
func TestSubscribeHeadsDeliversAndReconnects(t *testing.T) {
	head := func(sub string, b byte) string {
		return `{"jsonrpc":"2.0","method":"eth_subscription","params":{"subscription":"` + sub + `","result":{"number":"0x1","hash":"` + domain.Hash{b}.Hex() + `"}}}`
	}
	var dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		dial := dials.Add(1)
		_, request, err := conn.Read(r.Context())
		if err != nil || !strings.Contains(string(request), `"newHeads"`) {
			t.Error("subscription request", string(request), err)
			return
		}
		messages := []string{`{"jsonrpc":"2.0","id":1,"result":"0xabc"}`, head("0xother", 9), head("0xabc", byte(dial))}
		for _, m := range messages {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(m)); err != nil {
				return
			}
		}
		// Drop the first connection; keep the second open.
		if dial == 1 {
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got := make(chan domain.Hash, 4)
	go SubscribeHeads(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), func(h domain.Hash) { got <- h })
	for _, want := range []domain.Hash{{1}, {2}} {
		select {
		case h := <-got:
			if h != want {
				t.Fatal("head", h, "want", want)
			}
		case <-ctx.Done():
			t.Fatal("head not delivered", want)
		}
	}
}

// A failed dial names the host only: the path may hold a provider key.
func TestSubscribeHeadsDialErrorOmitsThePath(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/secret-key?token=hidden"
	server.Close()
	err := subscribeHeadsOnce(t.Context(), url, func(domain.Hash) {})
	if err == nil || strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "hidden") {
		t.Fatal("dial error leaks the endpoint path:", err)
	}
}
