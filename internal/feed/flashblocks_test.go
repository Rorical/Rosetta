package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/coder/websocket"
	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
)

func flashFixture() map[string]any {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte{1})
	var hash domain.Hash
	copy(hash[:], h.Sum(nil))
	return map[string]any{
		"payload_id": "0x0102030405060708", "index": 0,
		"base":     map[string]any{"parent_hash": "0x" + strings.Repeat("11", 32), "block_number": "0xb", "timestamp": "0x64"},
		"diff":     map[string]any{"transactions": []string{"0x01"}},
		"metadata": map[string]any{"block_number": 11, "receipts": map[string]any{hash.Hex(): map[string]any{"Eip1559": map[string]any{"status": "0x1", "logs": []any{map[string]any{"address": "0x" + strings.Repeat("22", 20), "topics": []string{"0x" + strings.Repeat("33", 32)}, "data": "0x1234"}}}}}},
	}
}

func TestFlashblocksReceiptsAndBrotli(t *testing.T) {
	encoded, _ := json.Marshal(flashFixture())
	var compressed bytes.Buffer
	writer := brotli.NewWriter(&compressed)
	writer.Write(encoded)
	writer.Close()
	for _, data := range [][]byte{encoded, compressed.Bytes()} {
		block, err := DecodeFlashblock(data)
		if err != nil || block.Number != 11 || block.Base.Number != 11 || len(block.Transactions) != 1 || len(block.Transactions[0].Logs) != 1 || !bytes.Equal(block.Transactions[0].Logs[0].Data, []byte{0x12, 0x34}) {
			t.Fatal(block, err)
		}
	}
}

func TestFlashblocksRejectIncompleteOrContradictoryEffects(t *testing.T) {
	for _, name := range []string{"stripped receipts", "wrong transaction hash", "missing logs", "revert effects", "missing base", "wrong height", "bad index", "bad topic", "duplicate transaction"} {
		t.Run(name, func(t *testing.T) {
			frame := flashFixture()
			metadata := frame["metadata"].(map[string]any)
			receipts := metadata["receipts"].(map[string]any)
			var receipt map[string]any
			for _, value := range receipts {
				receipt = value.(map[string]any)["Eip1559"].(map[string]any)
			}
			switch name {
			case "stripped receipts":
				delete(metadata, "receipts")
			case "wrong transaction hash":
				for key, value := range receipts {
					delete(receipts, key)
					receipts["0x"+strings.Repeat("44", 32)] = value
				}
			case "missing logs":
				delete(receipt, "logs")
			case "revert effects":
				receipt["status"] = "0x0"
			case "missing base":
				delete(frame, "base")
			case "wrong height":
				metadata["block_number"] = 12
			case "bad index":
				frame["index"] = -1
			case "bad topic":
				receipt["logs"].([]any)[0].(map[string]any)["topics"] = []string{"0x12"}
			case "duplicate transaction":
				frame["diff"].(map[string]any)["transactions"] = []string{"0x01", "0x01"}
			}
			encoded, _ := json.Marshal(frame)
			if _, err := DecodeFlashblock(encoded); err == nil {
				t.Fatal("invalid stream accepted")
			}
		})
	}
}

func TestFlashClientInvalidatesBeforeConnectingAndAfterDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var resets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if resets.Load() == 0 {
			t.Error("connection preceded prefix reset")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		data, _ := json.Marshal(flashFixture())
		conn.Write(ctx, websocket.MessageBinary, data)
	}))
	defer server.Close()
	seen := false
	err := (FlashClient{URL: "ws" + strings.TrimPrefix(server.URL, "http")}).Run(ctx, func() error {
		if resets.Add(1) > 1 {
			cancel()
		}
		return nil
	}, func(block Flashblock) error { seen = true; return nil })
	if err != nil || !seen || resets.Load() < 2 {
		t.Fatal("disconnect did not retire prefix", err, seen, resets.Load())
	}
}
