package feed

import (
	"context"
	"errors"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandshakeUsesSelectedChain(t *testing.T) {
	headers := http.Header{}
	headers.Set("Arbitrum-Feed-Server-Version", "2")
	headers.Set("Arbitrum-Chain-Id", "4663")
	if err := validateHandshake(headers, true, 4663); err != nil {
		t.Fatal(err)
	}
	if validateHandshake(headers, true, 42161) == nil || validateHandshake(headers, false, 42161) == nil {
		t.Fatal("cross-chain handshake accepted")
	}
}

func TestFastHandshakeRejectsWrongChainWithoutRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Arbitrum-Feed-Server-Version", "2")
		w.Header().Set("Arbitrum-Chain-Id", "42161")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	client, err := NewFastClient(feedURL(server), "test-key", ClientOptions{ChainID: 4663})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = client.Run(ctx, func(FastMessage) error { t.Error("wrong-chain event delivered"); return nil }, func() error { return nil })
	var mismatch *protocolError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected terminal chain mismatch, got %v", err)
	}
}
