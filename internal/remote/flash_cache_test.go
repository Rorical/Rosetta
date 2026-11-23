package remote

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func cachePrefix(t *testing.T) (flashPrefix, rpc.ExecutionBlock) {
	t.Helper()
	raw, err := os.ReadFile("../feed/testdata/base-flashblocks-index-zero.json")
	if err != nil {
		t.Fatal(err)
	}
	frame, err := feed.DecodeFlashblockEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	base := *frame.Base
	parent := rpc.ExecutionBlock{Ref: domain.BlockRef{Number: base.Number - 1, Hash: base.Parent, Timestamp: base.Timestamp - 2}, BaseFee: base.Execution.BaseFee, GasLimit: base.Execution.GasLimit}
	prefix := flashPrefix{Base: base, Payload: frame.PayloadID, Index: frame.Index, Transactions: frame.Transactions, GasUsed: frame.GasUsed, WithdrawalsRoot: frame.WithdrawalsRoot, BlobGasUsed: frame.BlobGasUsed, Source: domain.Hash{82}}
	prefix.Version = domain.StateVersion{Source: domain.SourceFast, Anchor: parent.Ref, BlockNumber: base.Number, TxIndex: uint64(len(prefix.Transactions) - 1), Epoch: 1, PrefixHash: domain.Hash{81}}
	return prefix, parent
}

func cacheExport(prefix flashPrefix, overlay map[string]any) cachedFlashState {
	e := prefix.Base.Execution
	encoded, _ := json.Marshal(overlay)
	var state map[string]json.RawMessage
	json.Unmarshal(encoded, &state)
	result := cachedFlashState{
		ChainID: "0x2105", PayloadID: prefix.Payload, Index: &prefix.Index,
		ParentHash: prefix.Base.Parent.Hex(), BlockNumber: fmt.Sprintf("0x%x", prefix.Base.Number),
		Timestamp: fmt.Sprintf("0x%x", prefix.Base.Timestamp), GasLimit: fmt.Sprintf("0x%x", e.GasLimit),
		GasUsed: fmt.Sprintf("0x%x", *prefix.GasUsed), BaseFee: "0x" + e.BaseFee.Text(16),
		Coinbase: e.Coinbase.Hex(), Random: e.Random.Hex(), BeaconRoot: e.BeaconRoot.Hex(),
		Extra: flashHex(e.Extra), WithdrawalsRoot: prefix.WithdrawalsRoot.Hex(), StateOverrides: state,
	}
	if prefix.BlobGasUsed != nil {
		text := fmt.Sprintf("0x%x", *prefix.BlobGasUsed)
		result.BlobGasUsed = &text
	}
	for i, tx := range prefix.Transactions {
		number, index := fmt.Sprintf("0x%x", prefix.Base.Number), fmt.Sprintf("0x%x", i)
		logs := []map[string]any{}
		for _, log := range tx.Logs {
			topics := []string{}
			for _, topic := range log.Topics {
				topics = append(topics, topic.Hex())
			}
			logs = append(logs, map[string]any{"address": log.Address.Hex(), "topics": topics, "data": flashHex(log.Data), "blockNumber": number, "transactionIndex": index, "transactionHash": tx.Hash.Hex()})
		}
		status := "0x0"
		if tx.Successful {
			status = "0x1"
		}
		receipt, _ := json.Marshal(map[string]any{"transactionHash": tx.Hash.Hex(), "blockNumber": number, "transactionIndex": index, "status": status, "logs": logs})
		result.Receipts = append(result.Receipts, receipt)
	}
	return result
}

func cacheServer(t *testing.T, cached any, parent rpc.ExecutionBlock, cacheCode int) (*rpc.Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var exports, replays atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&call)
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		switch call.Method {
		case flashCacheMethod:
			exports.Add(1)
			if cacheCode != 0 {
				response["error"] = map[string]any{"code": cacheCode, "message": "cache unavailable"}
			} else {
				response["result"] = cached
			}
		case "eth_getBlockByNumber":
			response["result"] = map[string]any{"number": fmt.Sprintf("0x%x", parent.Ref.Number), "hash": parent.Ref.Hash.Hex(), "parentHash": parent.Ref.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", parent.Ref.Timestamp), "baseFeePerGas": "0x" + parent.BaseFee.Text(16), "gasLimit": fmt.Sprintf("0x%x", parent.GasLimit), "miner": parent.Coinbase.Hex(), "transactions": []string{}}
		case "eth_getCode":
			// Replay cannot proceed without its verified system contracts. This
			// marker lets the portable test observe fallback without a fake EVM.
			replays.Add(1)
			response["error"] = map[string]any{"code": -32000, "message": "replay marker"}
		default:
			t.Errorf("unexpected RPC %s", call.Method)
			response["error"] = map[string]any{"code": -32601, "message": "unexpected method"}
		}
		json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return rpc.NewClient(server.URL, rpc.Options{}), &exports, &replays
}

func TestFlashCacheIdentityAndFallback(t *testing.T) {
	prefix, parent := cachePrefix(t)
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}
	for _, fault := range []string{"", "null", "index", "transactions", "receipt status", "receipt logs", "context", "missing state", "invalid state", "unavailable", "unsupported", "reorg"} {
		t.Run(fault, func(t *testing.T) {
			cached := cacheExport(prefix, map[string]any{})
			var result any = cached
			code := 0
			servedParent := parent
			switch fault {
			case "null":
				result = nil
			case "index":
				index := prefix.Index + 1
				cached.Index = &index
			case "transactions":
				cached.Receipts = cached.Receipts[:0]
			case "receipt status", "receipt logs":
				var receipt map[string]any
				json.Unmarshal(cached.Receipts[0], &receipt)
				if fault == "receipt status" {
					receipt["status"] = "0x1"
				} else {
					delete(receipt, "logs")
				}
				cached.Receipts[0], _ = json.Marshal(receipt)
			case "context":
				cached.Timestamp = "0x1"
			case "missing state":
				cached.StateOverrides = nil
			case "invalid state":
				cached.StateOverrides[addressAt(1).Hex()] = json.RawMessage(`{"state":{},"stateDiff":{}}`)
			case "unavailable":
				code = -32001
			case "unsupported":
				code = -32601
			case "reorg":
				servedParent.Ref.Hash = domain.Hash{99}
			}
			if fault != "null" {
				result = cached
			}
			client, exports, replays := cacheServer(t, result, servedParent, code)
			source := &flashSnapshotSource{}
			for i := 0; i < 2; i++ {
				view, proof, err := source.open(t.Context(), client, parent, chain, domain.Deployments{}, prefix)
				if fault == "" {
					if err != nil || proof.StateSource != "rpc-cache-v1" || view.blockID().(map[string]any)["blockHash"] != parent.Ref.Hash.Hex() {
						t.Fatal("cache snapshot not used", proof, err)
					}
					view.Close()
				} else if err == nil {
					t.Fatal("invalid cache accepted", fault)
				}
			}
			if fault == "unsupported" {
				if exports.Load() != 1 || replays.Load() != 2 {
					t.Fatal("unsupported capability was not cached", exports.Load(), replays.Load())
				}
			} else if exports.Load() != 2 || replays.Load() != 0 {
				t.Fatal("cache failure silently triggered replay", exports.Load(), replays.Load())
			}
		})
	}
}

func TestFlashCacheOverrideDecoding(t *testing.T) {
	address := addressAt(1).Hex()
	for _, entry := range []string{
		`null`, `{"balance":"-1"}`, `{"nonce":"0x10000000000000000"}`,
		`{"code":"0x0"}`, `{"stateDiff":{"0x01":"0x02"}}`,
		`{"stateDiff":{},"state":{}}`, `{"storage":{}}`,
	} {
		if _, err := decodeCachedOverrides(map[string]json.RawMessage{address: json.RawMessage(entry)}); err == nil {
			t.Fatal("invalid cached override accepted", entry)
		}
	}
	// A deletion must replace all storage, while a storage clear must retain
	// an explicit zero word. Dropping either changes the subsequent executor.
	deletion := json.RawMessage(`{"balance":"0x0","nonce":"0x0","code":"0x","state":{}}`)
	cleared := json.RawMessage(fmt.Sprintf(`{"stateDiff":{"%s":"%s"}}`, wordHex(9), wordHex(0)))
	decoded, err := decodeCachedOverrides(map[string]json.RawMessage{address: deletion, addressAt(2).Hex(): cleared})
	if err != nil {
		t.Fatal(err)
	}
	if storage, ok := decoded[address].(map[string]any)["state"].(map[string]string); !ok || len(storage) != 0 {
		t.Fatal("deletion lost its full storage replacement", decoded)
	}
	if decoded[addressAt(2).Hex()].(map[string]any)["stateDiff"].(map[string]string)[wordHex(9)] != wordHex(0) {
		t.Fatal("explicit cleared slot lost", decoded)
	}
}
