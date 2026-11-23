package rpc

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestGetLogsSuccess(t *testing.T) {
	emitter := mustAddress(t, "0x"+strings.Repeat("aa", 20))
	topic1 := mustHash(t, "0x"+strings.Repeat("01", 32))
	topic2 := mustHash(t, "0x"+strings.Repeat("02", 32))
	blockHash := mustHash(t, "0x"+strings.Repeat("bb", 32))
	txHash := mustHash(t, "0x"+strings.Repeat("cc", 32))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		if request.Method != "eth_getLogs" {
			t.Errorf("method = %q, want eth_getLogs", request.Method)
		}
		if len(request.Params) != 1 {
			t.Errorf("params = %v, want one filter object", request.Params)
		}
		writeResult(t, w, request.ID, []any{
			map[string]any{
				"address":         emitter.Hex(),
				"topics":          []string{topic1.Hex(), topic2.Hex()},
				"data":            "0xdeadbeef",
				"blockNumber":     "0x10",
				"blockHash":       blockHash.Hex(),
				"transactionHash": txHash.Hex(),
				"logIndex":        "0x2",
				"removed":         false,
			},
			map[string]any{
				"address":  emitter.Hex(),
				"topics":   []string{topic1.Hex()},
				"data":     "0x",
				"logIndex": "0x3",
				"removed":  true,
			},
		})
	}))
	defer server.Close()

	logs, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{
		FromBlock: 0x10,
		ToBlock:   0x20,
	})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1 (removed log skipped)", len(logs))
	}
	got := logs[0]
	if got.Address != emitter {
		t.Fatalf("address = %s, want %s", got.Address, emitter)
	}
	if len(got.Topics) != 2 || got.Topics[0] != topic1 || got.Topics[1] != topic2 {
		t.Fatalf("topics = %v, want [%s %s]", got.Topics, topic1.Hex(), topic2.Hex())
	}
	if !bytes.Equal(got.Data, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("data = %x, want deadbeef", got.Data)
	}
	if got.BlockNumber != 0x10 || got.BlockHash != blockHash || got.TxHash != txHash {
		t.Fatalf("block fields = %d/%s/%s, want 16/%s/%s",
			got.BlockNumber, got.BlockHash.Hex(), got.TxHash.Hex(), blockHash.Hex(), txHash.Hex())
	}
	if got.LogIndex != 2 || got.Removed {
		t.Fatalf("index = %d, removed = %v, want 2, false", got.LogIndex, got.Removed)
	}
}

func TestGetLogsFilterParams(t *testing.T) {
	first := mustAddress(t, "0x"+strings.Repeat("11", 20))
	second := mustAddress(t, "0x"+strings.Repeat("22", 20))
	topic := mustHash(t, "0x"+strings.Repeat("33", 32))
	otherTopic := mustHash(t, "0x"+strings.Repeat("44", 32))

	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		filter, ok := request.Params[0].(map[string]any)
		if !ok {
			t.Errorf("filter = %v, want object", request.Params[0])
			return
		}
		seen = filter
		writeResult(t, w, request.ID, []any{})
	}))
	defer server.Close()

	logs, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{
		FromBlock: 0x100,
		ToBlock:   0x1ff,
		Addresses: []domain.Address{first, second},
		Topics:    [][]domain.Hash{{topic, otherTopic}, nil},
	})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("len(logs) = %d, want 0", len(logs))
	}
	if seen["fromBlock"] != "0x100" || seen["toBlock"] != "0x1ff" {
		t.Fatalf("block range = %v-%v, want 0x100-0x1ff", seen["fromBlock"], seen["toBlock"])
	}
	addresses, ok := seen["address"].([]any)
	if !ok || len(addresses) != 2 || addresses[0] != first.Hex() || addresses[1] != second.Hex() {
		t.Fatalf("address = %v, want [%s %s]", seen["address"], first.Hex(), second.Hex())
	}
	topics, ok := seen["topics"].([]any)
	if !ok || len(topics) != 2 {
		t.Fatalf("topics = %v, want two positions", seen["topics"])
	}
	position, ok := topics[0].([]any)
	if !ok || len(position) != 2 || position[0] != topic.Hex() || position[1] != otherTopic.Hex() {
		t.Fatalf("topics[0] = %v, want [%s %s]", topics[0], topic.Hex(), otherTopic.Hex())
	}
	if topics[1] != nil {
		t.Fatalf("topics[1] = %v, want null wildcard", topics[1])
	}
}

func TestGetLogsNullResult(t *testing.T) {
	server := resultServer(t, nil)
	logs, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{FromBlock: 1, ToBlock: 2})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if logs == nil || len(logs) != 0 {
		t.Fatalf("logs = %v, want empty non-nil slice", logs)
	}
}

func TestGetLogsEmptyResult(t *testing.T) {
	server := resultServer(t, []any{})
	logs, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{FromBlock: 1, ToBlock: 2})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("len(logs) = %d, want 0", len(logs))
	}
}

func TestGetLogsReversedRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("reversed range reached the server")
	}))
	defer server.Close()

	_, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{FromBlock: 5, ToBlock: 4})
	if err == nil {
		t.Fatal("GetLogs succeeded, want range error")
	}
}

func TestGetLogsMalformed(t *testing.T) {
	address := "0x" + strings.Repeat("aa", 20)
	topic := "0x" + strings.Repeat("01", 32)

	tests := []struct {
		name string
		log  map[string]any
	}{
		{
			name: "address",
			log:  map[string]any{"address": "0x1234", "data": "0x", "logIndex": "0x0"},
		},
		{
			name: "topic",
			log:  map[string]any{"address": address, "topics": []string{"0xzz"}, "data": "0x", "logIndex": "0x0"},
		},
		{
			name: "data",
			log:  map[string]any{"address": address, "topics": []string{topic}, "data": "0xzz", "logIndex": "0x0"},
		},
		{
			name: "missing address",
			log:  map[string]any{"topics": []string{topic}, "data": "0x", "logIndex": "0x0"},
		},
		{
			name: "missing data",
			log:  map[string]any{"address": address, "topics": []string{topic}, "logIndex": "0x0"},
		},
		{
			name: "missing logIndex",
			log:  map[string]any{"address": address, "topics": []string{topic}, "data": "0x"},
		},
		{
			name: "logIndex",
			log:  map[string]any{"address": address, "topics": []string{topic}, "data": "0x", "logIndex": "12"},
		},
		{
			name: "blockNumber",
			log: map[string]any{
				"address": address, "topics": []string{topic}, "data": "0x", "logIndex": "0x0",
				"blockNumber": "nope",
			},
		},
		{
			name: "blockHash",
			log: map[string]any{
				"address": address, "topics": []string{topic}, "data": "0x", "logIndex": "0x0",
				"blockHash": "0x12",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := resultServer(t, []any{test.log})
			_, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{FromBlock: 1, ToBlock: 2})
			if err == nil {
				t.Fatal("GetLogs succeeded, want malformed log error")
			}
		})
	}
}

func TestGetLogsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{FromBlock: 1, ToBlock: 2})
	if err == nil {
		t.Fatal("GetLogs succeeded, want protocol error")
	}
}

func TestGetLogsJSONRPCError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"range too wide"}}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{FromBlock: 1, ToBlock: 2})
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32005 {
		t.Fatalf("error = %v, want *RPCError with code -32005", err)
	}
}

func TestGetLogsMultipleEntries(t *testing.T) {
	emitter := mustAddress(t, "0x"+strings.Repeat("aa", 20))
	topic := mustHash(t, "0x"+strings.Repeat("01", 32))
	server := resultServer(t, []any{
		map[string]any{
			"address": emitter.Hex(), "topics": []string{topic.Hex()},
			"data": "0x01", "logIndex": "0x0",
		},
		map[string]any{
			"address": emitter.Hex(), "topics": []string{topic.Hex()},
			"data": "0x02", "logIndex": "0x1",
		},
	})

	logs, err := NewClient(server.URL, Options{}).GetLogs(context.Background(), LogFilter{FromBlock: 1, ToBlock: 2})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("len(logs) = %d, want 2", len(logs))
	}
	if logs[0].LogIndex != 0 || logs[1].LogIndex != 1 {
		t.Fatalf("indices = %d/%d, want 0/1", logs[0].LogIndex, logs[1].LogIndex)
	}
	if !bytes.Equal(logs[0].Data, []byte{0x01}) || !bytes.Equal(logs[1].Data, []byte{0x02}) {
		t.Fatalf("data = %x/%x, want 01/02", logs[0].Data, logs[1].Data)
	}
}
