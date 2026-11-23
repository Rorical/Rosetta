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

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type opRPC struct {
	fixture *opFixture
	fault   atomic.Int32
	replay  bool
	paper   bool
}

func (node *opRPC) server(t *testing.T) *rpc.Client {
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
func (node *opRPC) respond(_ context.Context, method string, params []json.RawMessage) (any, error) {
	f := node.fixture
	if method != "eth_chainId" && method != "eth_getBlockByNumber" && method != "eth_getBlockByHash" {
		var position struct {
			BlockHash        string
			RequireCanonical bool
		}
		if err := json.Unmarshal(params[1], &position); err != nil || position.BlockHash != (domain.Hash{10}).Hex() || !position.RequireCanonical {
			return nil, fmt.Errorf("read escaped fixed canonical hash")
		}
	}
	switch method {
	case "eth_chainId":
		return "0x2105", nil
	case "eth_getBlockByNumber", "eth_getBlockByHash":
		hash := domain.Hash{10}
		if node.fault.Load() == 3 {
			hash = domain.Hash{99}
		}
		return map[string]any{"number": "0xa", "hash": hash.Hex(), "parentHash": (domain.Hash{9}).Hex(), "timestamp": "0x3e8", "gasLimit": "0x1e84800", "baseFeePerGas": "0x1", "miner": (domain.Address{}).Hex(), "transactions": []string{}}, nil
	case "eth_getCode", "eth_getBalance", "eth_getTransactionCount":
		var text string
		json.Unmarshal(params[0], &text)
		if text == opGasOracle.Hex() && method == "eth_getCode" {
			return "0x00", nil
		}
		var account struct {
			Code, Balance string
			Nonce         uint64
		}
		if err := f.engine.request(map[string]any{"method": "account", "address": text}, &account); err != nil {
			return nil, err
		}
		if method == "eth_getCode" {
			return account.Code, nil
		}
		if method == "eth_getBalance" {
			if node.fault.Load() == 6 {
				return "0x0", nil
			}
			return account.Balance, nil
		}
		return fmt.Sprintf("0x%x", account.Nonce), nil
	case "eth_call":
		var call map[string]any
		json.Unmarshal(params[0], &call)
		target, _ := call["to"].(string)
		data, _ := call["data"].(string)
		if target == opL1Block.Hex() {
			return abiHex(100), nil
		}
		if target == opGasOracle.Hex() {
			if node.fault.Load() == 4 {
				return nil, fmt.Errorf("operator/data API unsupported")
			}
			word, err := hexData(data)
			if err != nil || len(word) < 4 {
				return nil, fmt.Errorf("bad oracle ABI")
			}
			value := new(big.Int).SetBytes(word[4:]).Uint64()
			switch data[:10] {
			case "0x" + fmt.Sprintf("%x", selector("l1BaseFee()")):
				return abiHex(1_000_000_000), nil
			case "0x" + fmt.Sprintf("%x", selector("getOperatorFee(uint256)")):
				if f.fork == "isthmus" {
					return abiHex(int64(value*2/1_000_000 + 50)), nil
				}
				return abiHex(int64(value*200 + 50)), nil
			case "0x" + fmt.Sprintf("%x", selector("getL1FeeUpperBound(uint256)")):
				size := value + 68
				flz := size + size/255 + 16
				estimated := new(big.Int).SetUint64(max(100_000_000, flz*836_500-42_585_600))
				fee := estimated.Mul(estimated, big.NewInt(16_000_000_001_000))
				fee.Quo(fee, big.NewInt(1_000_000_000_000))
				return "0x" + fmt.Sprintf("%064x", fee), nil
			}
			return nil, fmt.Errorf("unknown oracle selector")
		}
		if call["from"] == nil {
			call["from"] = f.owner.Hex()
		}
		call["gas"] = "0x800000"
		call["maxFeePerGas"] = "0x2"
		var overrides any
		if len(params) > 2 {
			json.Unmarshal(params[2], &overrides)
		}
		var block map[string]any
		if len(params) > 3 {
			json.Unmarshal(params[3], &block)
		}
		result, err := f.executeAt(call, overrides, false, false, block)
		if err != nil {
			return nil, err
		}
		if !result.Success {
			return nil, fmt.Errorf("OP read reverted %s", result.Output)
		}
		return result.Output, nil
	case "debug_traceCall":
		var call map[string]any
		json.Unmarshal(params[0], &call)
		if call["type"] != "0x2" || call["chainId"] != "0x2105" {
			return nil, fmt.Errorf("trace did not encode OP envelope identity")
		}
		var options struct {
			Tracer         string
			StateOverrides map[string]any
			BlockOverrides map[string]any
		}
		json.Unmarshal(params[2], &options)
		if !node.paper && !node.replay && len(options.StateOverrides) != 1 {
			return nil, fmt.Errorf("only sender fee prepayment may be overridden")
		}
		sender, _ := call["from"].(string)
		reserve, ok := options.StateOverrides[sender].(map[string]any)
		if !ok || !node.paper && !node.replay && len(reserve) != 1 || reserve["balance"] == nil {
			return nil, fmt.Errorf("trace changed code, nonce or storage")
		}
		if node.replay && (options.BlockOverrides["number"] != "0xb" || options.BlockOverrides["time"] != "0x3ea") {
			return nil, fmt.Errorf("Flashblocks trace lost child block context")
		}
		result, err := f.executeAt(call, options.StateOverrides, false, node.fault.Load() == 1, options.BlockOverrides)
		if err != nil {
			return nil, err
		}
		switch options.Tracer {
		case "callTracer":
			frame := map[string]any{"output": result.Output, "gasUsed": result.GasUsed}
			if !result.Success {
				frame["error"] = "execution reverted"
				if node.fault.Load() == 7 {
					delete(frame, "output")
				}
			}
			return frame, nil
		case "prestateTracer":
			if node.fault.Load() == 5 {
				balance, err := integer(result.Post[sender]["balance"].(string))
				if err != nil {
					return nil, err
				}
				gas, _ := quantity(result.GasUsed)
				limit, _ := quantity(call["gas"].(string))
				balance.Sub(balance, new(big.Int).SetUint64(limit-gas))
				result.Post[sender]["balance"] = "0x" + balance.Text(16)
			}
			if node.fault.Load() == 2 {
				result.Post[sender]["nonce"] = float64(0)
			}
			return map[string]any{"pre": result.Pre, "post": result.Post}, nil
		}
	}
	return nil, fmt.Errorf("unsupported OP test RPC %s", method)
}
func abiHex(value int64) string { return "0x" + fmt.Sprintf("%064x", big.NewInt(value)) }
