//go:build opnative && contracts

package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type flashRPC struct {
	base         *opRPC
	fault        string
	cached       *cachedFlashState
	nativeTraces atomic.Int32
}

func (node *flashRPC) server(t *testing.T) *rpc.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		value, err := node.respond(r.Context(), call.Method, call.Params)
		result := map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": value}
		if err != nil {
			delete(result, "result")
			result["error"] = map[string]any{"code": -32000, "message": err.Error()}
		}
		if err := json.NewEncoder(w).Encode(result); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return rpc.NewClient(server.URL, rpc.Options{})
}
func (node *flashRPC) respond(ctx context.Context, method string, params []json.RawMessage) (any, error) {
	if method == flashCacheMethod && node.cached != nil {
		return node.cached, nil
	}
	if method != "debug_traceBlock" {
		return node.base.respond(ctx, method, params)
	}
	node.nativeTraces.Add(1)
	var text string
	if err := json.Unmarshal(params[0], &text); err != nil {
		return nil, err
	}
	raw, err := hexData(text)
	if err != nil {
		return nil, err
	}
	var block struct {
		Header       flashHeader
		Transactions []rlp.RawValue
		Uncles       []rlp.RawValue
		Withdrawals  []rlp.RawValue
	}
	if err := rlp.DecodeBytes(raw, &block); err != nil {
		return nil, err
	}
	if block.Header.Number != 11 || block.Header.Time != 1002 || block.Header.ParentHash != (domain.Hash{10}) || block.Header.GasLimit != 32_000_000 {
		return nil, fmt.Errorf("replay escaped intended child execution context")
	}
	var options struct {
		Tracer       string
		TracerConfig map[string]json.RawMessage
	}
	if err := json.Unmarshal(params[1], &options); err != nil {
		return nil, err
	}
	if options.Tracer != "muxTracer" || options.TracerConfig["callTracer"] == nil || options.TracerConfig["prestateTracer"] == nil {
		return nil, fmt.Errorf("missing native replay tracers")
	}
	f := node.base.fixture
	if err := f.engine.request(map[string]any{"method": "checkpoint"}, nil); err != nil {
		return nil, err
	}
	defer f.engine.request(map[string]any{"method": "restore"}, nil)
	var traces []map[string]any
	for _, encoded := range block.Transactions {
		var transaction []byte
		if err := rlp.DecodeBytes(encoded, &transaction); err != nil {
			return nil, err
		}
		call, err := decodeNativePrefixCall(transaction)
		if err != nil {
			return nil, err
		}
		result, err := f.executeAt(call, nil, true, true, map[string]any{"number": "0xb", "time": "0x3ea"})
		if err != nil {
			return nil, err
		}
		frame := map[string]any{"gasUsed": result.GasUsed, "logs": result.Logs}
		if !result.Success {
			frame["error"] = "execution reverted"
		}
		traces = append(traces, map[string]any{"txHash": digest(transaction).Hex(), "result": map[string]any{"callTracer": frame, "prestateTracer": map[string]any{"pre": result.Pre, "post": result.Post}}})
	}
	switch node.fault {
	case "count":
		return traces[:len(traces)-1], nil
	case "hash":
		traces[0]["txHash"] = (domain.Hash{99}).Hex()
	case "status":
		traces[0]["result"].(map[string]any)["callTracer"].(map[string]any)["error"] = "execution reverted"
	case "diff":
		delete(traces[0]["result"].(map[string]any), "prestateTracer")
	case "logs":
		traces[0]["result"].(map[string]any)["callTracer"].(map[string]any)["logs"] = []any{map[string]any{"address": addressAt(1).Hex(), "topics": []string{}, "data": "0x", "position": "0x0"}}
	}
	return traces, nil
}

// Decode and recover real type-2 envelopes in the test RPC. Deposits retain
// their source hash, mint and privileged sender in native OP execution.
func decodeNativePrefixCall(raw []byte) (map[string]any, error) {
	if len(raw) < 2 {
		return nil, fmt.Errorf("short transaction")
	}
	var fields []rlp.RawValue
	if err := rlp.DecodeBytes(raw[1:], &fields); err != nil {
		return nil, err
	}
	dataAt := func(index int) ([]byte, error) {
		var v []byte
		err := rlp.DecodeBytes(fields[index], &v)
		return v, err
	}
	wordAt := func(index int) (*big.Int, error) { v, err := dataAt(index); return new(big.Int).SetBytes(v), err }
	call := map[string]any{"type": fmt.Sprintf("0x%x", raw[0]), "envelope": flashHex(raw), "value": "0x0", "maxFeePerGas": "0x2", "maxPriorityFeePerGas": "0x0"}
	switch raw[0] {
	case 0x7e:
		if len(fields) != 8 {
			return nil, fmt.Errorf("invalid deposit")
		}
		for field, index := range map[string]int{"sourceHash": 0, "from": 1, "to": 2, "data": 7} {
			v, err := dataAt(index)
			if err != nil {
				return nil, err
			}
			call[field] = flashHex(v)
		}
		for field, index := range map[string]int{"mint": 3, "value": 4, "gas": 5} {
			v, err := wordAt(index)
			if err != nil {
				return nil, err
			}
			call[field] = "0x" + v.Text(16)
		}
		call["maxFeePerGas"] = "0x0"
	case 2:
		if len(fields) != 12 {
			return nil, fmt.Errorf("invalid dynamic-fee transaction")
		}
		payload, err := rlp.EncodeToBytes(fields[:9])
		if err != nil {
			return nil, err
		}
		hash := digest(append([]byte{2}, payload...))
		signature := make([]byte, 65)
		for index, destination := range map[int][]byte{10: signature[:32], 11: signature[32:64]} {
			v, err := wordAt(index)
			if err != nil {
				return nil, err
			}
			v.FillBytes(destination)
		}
		parity, err := wordAt(9)
		if err != nil || parity.Uint64() > 1 {
			return nil, fmt.Errorf("bad parity")
		}
		signature[64] = byte(parity.Uint64())
		key, err := crypto.SigToPub(hash[:], signature)
		if err != nil {
			return nil, err
		}
		call["from"] = crypto.PubkeyToAddress(*key).Hex()
		for field, index := range map[string]int{"chainId": 0, "nonce": 1, "maxPriorityFeePerGas": 2, "maxFeePerGas": 3, "gas": 4, "value": 6} {
			v, err := wordAt(index)
			if err != nil {
				return nil, err
			}
			call[field] = "0x" + v.Text(16)
		}
		for field, index := range map[string]int{"to": 5, "data": 7} {
			v, err := dataAt(index)
			if err != nil {
				return nil, err
			}
			call[field] = flashHex(v)
		}
	default:
		return nil, fmt.Errorf("test prefix has unsupported type")
	}
	return call, nil
}
