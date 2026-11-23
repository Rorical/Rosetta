package remote

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// reactiveContextNode serves the executor, fee oracle and canonical headers
// read by a reactive context refresh, and counts calls by method/selector.
type reactiveContextNode struct {
	executor domain.Address
	code     []byte
	deployed uint64 // the executor has no code before this block
	head     uint64 // block number answered for "latest"
	version  atomic.Uint64
	fee      atomic.Uint64 // every OP fee oracle sample
	unserved atomic.Int32  // requests answered as if the block were unknown
	mu       sync.Mutex
	calls    map[string]int
	requests int // HTTP requests; a batch counts once
}

func (n *reactiveContextNode) count() (map[string]int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	out, requests := n.calls, n.requests
	n.calls, n.requests = map[string]int{}, 0
	return out, requests
}

func (n *reactiveContextNode) server(t *testing.T) *rpc.Client {
	t.Helper()
	names := map[string]string{}
	for _, signature := range []string{"paused()", "operators(address)", "chainId()", "configVersion()", "getL1FeeUpperBound(uint256)", "getOperatorFee(uint256)", "getPricesInWei()"} {
		names[hex.EncodeToString(selector(signature))] = signature
	}
	word := func(value uint64) string { return fmt.Sprintf("0x%064x", value) }
	type request struct {
		ID     uint64
		Method string
		Params []json.RawMessage
	}
	// Test block hashes carry the number in their first two bytes.
	blockOf := func(params []json.RawMessage) uint64 {
		if len(params) < 2 {
			return n.head
		}
		var tag struct {
			BlockHash string `json:"blockHash"`
		}
		var text string
		if json.Unmarshal(params[1], &tag) == nil && tag.BlockHash != "" {
			raw, _ := hex.DecodeString(strings.TrimPrefix(tag.BlockHash, "0x"))
			return uint64(raw[0]) | uint64(raw[1])<<8
		}
		if json.Unmarshal(params[1], &text) == nil && strings.HasPrefix(text, "0x") {
			var number uint64
			fmt.Sscanf(text, "0x%x", &number)
			return number
		}
		return n.head
	}
	answer := func(call request, unserved bool) map[string]any {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		if unserved {
			response["error"] = map[string]any{"code": -32000, "message": "header not found"}
			return response
		}
		name := call.Method
		missing := blockOf(call.Params) < n.deployed
		switch call.Method {
		case "eth_getBlockByNumber":
			hash := domain.Hash{byte(n.head), byte(n.head >> 8)}
			response["result"] = map[string]any{"number": fmt.Sprintf("0x%x", n.head), "hash": hash.Hex(), "parentHash": domain.Hash{}.Hex(), "timestamp": "0x1", "baseFeePerGas": fmt.Sprintf("0x%x", n.head), "gasLimit": "0x1c9c380", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		case "eth_getCode":
			response["result"] = "0x" + hex.EncodeToString(n.code)
			if missing {
				response["result"] = "0x"
			}
		case "eth_getBalance":
			response["result"] = "0xde0b6b3a7640000"
		case "eth_call":
			var request struct {
				To    string `json:"to"`
				Data  string `json:"data"`
				Input string `json:"input"`
			}
			json.Unmarshal(call.Params[0], &request)
			data := request.Data + request.Input
			name = names[strings.TrimPrefix(data, "0x")[:8]]
			switch name {
			case "paused()":
				response["result"] = word(0)
			case "operators(address)":
				response["result"] = word(1)
			case "chainId()":
				response["result"] = word(10)
			case "configVersion()":
				response["result"] = word(n.version.Load())
				if missing {
					response["result"] = "0x"
				}
			case "getL1FeeUpperBound(uint256)", "getOperatorFee(uint256)":
				response["result"] = word(n.fee.Load())
			case "getPricesInWei()":
				// perL2Tx, perL1CalldataByte, then four L2 prices.
				fee := n.fee.Load()
				response["result"] = word(fee*100) + strings.Repeat(fmt.Sprintf("%064x", fee), 5)
			default:
				t.Errorf("unexpected eth_call %s", data)
				response["error"] = map[string]any{"code": -32000, "message": "unexpected call"}
			}
		default:
			t.Errorf("unexpected RPC %s", call.Method)
			response["error"] = map[string]any{"code": -32601, "message": "unexpected method"}
		}
		n.mu.Lock()
		n.calls[name]++
		n.mu.Unlock()
		return response
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		n.mu.Lock()
		n.requests++
		n.mu.Unlock()
		unserved := n.unserved.Add(-1) >= 0
		if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
			var calls []request
			json.Unmarshal(raw, &calls)
			responses := make([]map[string]any, len(calls))
			for i, call := range calls {
				responses[i] = answer(call, unserved)
			}
			json.NewEncoder(w).Encode(responses)
			return
		}
		var call request
		json.Unmarshal(raw, &call)
		json.NewEncoder(w).Encode(answer(call, unserved))
	}))
	t.Cleanup(server.Close)
	return rpc.NewClient(server.URL, rpc.Options{})
}

func TestReactiveRefreshRereadsOnlyWhatChanged(t *testing.T) {
	node := &reactiveContextNode{executor: domain.Address{0xe0}, code: []byte{0x60, 0x00}, calls: map[string]int{}}
	node.version.Store(7)
	node.fee.Store(10)
	client := node.server(t)
	token := domain.Address{0x70}
	settings := reactiveSettings{
		document: reactivePolicy{MaxCalldataBytes: 2048, ContextMaxAgeMS: 5000},
		planning: planning.ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{0x5e}, Executor: node.executor, CodeHash: digest(node.code), GasLimit: 1_000_000},
		limits:   execution.DispatchLimits{PerTransaction: big.NewInt(1), Assets: map[domain.Address]*big.Int{token: big.NewInt(1000)}},
		minimum:  map[domain.Address]*big.Int{token: big.NewInt(1)},
		wallet:   map[domain.Address]bool{token: false},
	}
	s := &reactiveRuntime{client: client, settings: settings, accountBlocks: 150}
	block := func(number uint64) rpc.ExecutionBlock {
		return rpc.ExecutionBlock{Ref: domain.BlockRef{Number: number, Hash: domain.Hash{byte(number), byte(number >> 8)}}, BaseFee: new(big.Int).SetUint64(number)}
	}
	configuration := []string{"eth_getCode", "paused()", "operators(address)", "chainId()"}
	account := []string{"configVersion()", "eth_getBalance"}
	fees := []string{"getL1FeeUpperBound(uint256)", "getOperatorFee(uint256)"}
	refresh := func(number uint64, feeParams string) (*reactiveCache, map[string]int, int) {
		t.Helper()
		cache, err := s.refresh(t.Context(), block(number), opFeeParams{key: feeParams})
		if err != nil {
			t.Fatal(err)
		}
		if cache.block.Number != number || cache.costs.BaseFee.Uint64() != number {
			t.Fatal("context does not describe the refreshed header", cache.block, cache.costs.BaseFee)
		}
		calls, requests := node.count()
		return cache, calls, requests
	}
	expect := func(step string, calls map[string]int, read, skipped []string) {
		t.Helper()
		for _, name := range read {
			if calls[name] == 0 {
				t.Fatalf("%s skipped %s: %v", step, name, calls)
			}
		}
		for _, name := range skipped {
			if calls[name] != 0 {
				t.Fatalf("%s reread %s: %v", step, name, calls)
			}
		}
	}
	extra := func(cache *reactiveCache, sample uint64) {
		t.Helper()
		// Two times the L1 and operator samples at the largest size and gas.
		if cache.costs.Extra.Uint64() != 4*sample || cache.costs.L1.Y1.Uint64() != sample || cache.costs.Operator.Y0.Uint64() != sample {
			t.Fatal("fee context does not match the oracle", cache.costs.Extra, sample)
		}
	}

	first, calls, _ := refresh(100, "a")
	expect("startup", calls, append(append(configuration, account...), fees...), nil)
	extra(first, 10)

	// Same L1 origin and fee parameters, no landing of ours: the header alone.
	same, calls, requests := refresh(101, "a")
	if requests != 0 {
		t.Fatalf("unchanged block read the chain in %d requests: %v", requests, calls)
	}
	extra(same, 10)

	// A new L1 origin rereads only the fee samples, in one request.
	node.fee.Store(20)
	moved, calls, requests := refresh(102, "b")
	if requests != 1 {
		t.Fatalf("fee change used %d requests: %v", requests, calls)
	}
	expect("fee change", calls, fees, append(configuration, account...))
	extra(moved, 20)

	// A block without a user transaction cannot prove the parameters held.
	node.fee.Store(30)
	unknown, calls, _ := refresh(103, "")
	expect("unknown fee parameters", calls, fees, account)
	extra(unknown, 30)
	if _, calls, _ = refresh(104, "b"); calls["getL1FeeUpperBound(uint256)"] == 0 {
		t.Fatal("fingerprint b after an unknown block reused samples read under other parameters", calls)
	}

	// Our transaction landed: the account is reread, the fees are not.
	s.accountStale.Store(true)
	_, calls, requests = refresh(105, "b")
	if requests != 1 {
		t.Fatalf("account reread used %d requests: %v", requests, calls)
	}
	expect("own landing", calls, account, append(configuration, fees...))

	// The owner changed the executor: verification runs again.
	node.version.Store(8)
	s.accountStale.Store(true)
	_, calls, _ = refresh(106, "b")
	expect("config change", calls, append(configuration, account...), fees)

	// Outside transfers are picked up on the slow timer.
	if _, calls, requests = refresh(105+s.accountBlocks-1, "b"); requests != 0 {
		t.Fatalf("account read before its interval: %v", calls)
	}
	_, calls, _ = refresh(106+s.accountBlocks, "b")
	expect("account interval", calls, account, append(configuration, fees...))

	// A failed refresh forgets its reads; the next one starts over.
	s.settings.limits.PerTransaction = new(big.Int).Lsh(big.NewInt(1), 100)
	if _, err := s.refresh(t.Context(), block(400), opFeeParams{key: "b"}); err == nil {
		t.Fatal("refresh accepted a wallet below the transaction budget")
	}
	node.count()
	s.settings.limits.PerTransaction = big.NewInt(1)
	_, calls, _ = refresh(401, "b")
	expect("after failure", calls, append(account, fees...), configuration)
}

// Nitro charges L1 as poster gas, priced by ArbGasInfo. The price is reread
// every reactivePosterSeconds of block time, not every block, and no OP fee
// oracle is called.
func TestReactiveRefreshReadsNitroPosterPrices(t *testing.T) {
	node := &reactiveContextNode{executor: domain.Address{0xe0}, code: []byte{0x60, 0x00}, calls: map[string]int{}}
	node.version.Store(7)
	node.fee.Store(10)
	client := node.server(t)
	settings := reactiveSettings{
		document: reactivePolicy{MaxCalldataBytes: 2048, ContextMaxAgeMS: 5000},
		planning: planning.ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 10}, Sender: domain.Address{0x5e}, Executor: node.executor, CodeHash: digest(node.code), GasLimit: 1_000_000},
		limits:   execution.DispatchLimits{PerTransaction: big.NewInt(1)},
	}
	s := &reactiveRuntime{client: client, settings: settings, accountBlocks: 150}
	refresh := func(number, timestamp uint64) *reactiveCache {
		t.Helper()
		block := rpc.ExecutionBlock{Ref: domain.BlockRef{Number: number, Hash: domain.Hash{byte(number), byte(number >> 8)}, Timestamp: timestamp}, BaseFee: big.NewInt(1000)}
		cache, err := s.refresh(t.Context(), block, opFeeParams{})
		if err != nil {
			t.Fatal(err)
		}
		return cache
	}
	first := refresh(100, 1000)
	if calls, _ := node.count(); calls["getPricesInWei()"] != 1 || calls["getL1FeeUpperBound(uint256)"] != 0 {
		t.Fatalf("startup reads %v", calls)
	}
	if p := first.costs.Poster; p == nil || p.PerTx.Uint64() != 1000 || p.PerByte.Uint64() != 10 || first.costs.Extra.Sign() != 0 {
		t.Fatalf("poster price %+v, extra %s", first.costs.Poster, first.costs.Extra)
	}
	node.fee.Store(20)
	if same := refresh(101, 1000+reactivePosterSeconds-1); same.costs.Poster.PerByte.Uint64() != 10 {
		t.Fatal("price reread within its interval")
	}
	if calls, requests := node.count(); requests != 0 {
		t.Fatalf("unchanged context read the chain: %v", calls)
	}
	if moved := refresh(102, 1000+reactivePosterSeconds); moved.costs.Poster.PerByte.Uint64() != 20 {
		t.Fatal("price not reread after its interval")
	}
}

// A resume snapshot can predate the configured executor: the operator
// deployed it while the bot was down. Startup must not fail on the
// snapshot block; the context describes the account now, at the head.
func TestReactiveStartContextFallsBackToTheHead(t *testing.T) {
	node := &reactiveContextNode{executor: domain.Address{0xe0}, code: []byte{0x60, 0x00}, deployed: 200, head: 250, calls: map[string]int{}}
	node.version.Store(2)
	node.fee.Store(10)
	client := node.server(t)
	token := domain.Address{0x70}
	s := &reactiveRuntime{client: client, settings: reactiveSettings{
		document: reactivePolicy{MaxCalldataBytes: 2048, ContextMaxAgeMS: 5000},
		planning: planning.ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{0x5e}, Executor: node.executor, CodeHash: digest(node.code), GasLimit: 1_000_000},
		limits:   execution.DispatchLimits{PerTransaction: big.NewInt(1), Assets: map[domain.Address]*big.Int{token: big.NewInt(1000)}},
		minimum:  map[domain.Address]*big.Int{token: big.NewInt(1)},
		wallet:   map[domain.Address]bool{token: false},
	}}
	resumed := rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 100, Hash: domain.Hash{100}}, BaseFee: big.NewInt(100)}
	if _, err := s.refresh(t.Context(), resumed, opFeeParams{key: "a"}); err == nil {
		t.Fatal("fixture: the executor should be missing at the resumed block")
	}
	cache, err := s.startContext(t.Context(), resumed, opFeeParams{key: "a"})
	if err != nil {
		t.Fatal("startup failed on a snapshot older than the executor:", err)
	}
	if cache.block.Number != 250 {
		t.Fatalf("context block %d, want the head 250", cache.block.Number)
	}
}

// The follower can take a block from the flashblock stream before the RPC
// serves it; the context refresh retries instead of leaving the hot path
// without a context for the whole block.
func TestReactiveRefreshRetriesABlockTheRPCDoesNotServeYet(t *testing.T) {
	node := &reactiveContextNode{executor: domain.Address{0xe0}, code: []byte{0x60, 0x00}, calls: map[string]int{}}
	node.version.Store(7)
	node.fee.Store(10)
	node.unserved.Store(2)
	settings := reactiveSettings{
		document: reactivePolicy{MaxCalldataBytes: 2048, ContextMaxAgeMS: 5000},
		planning: planning.ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{0x5e}, Executor: node.executor, CodeHash: digest(node.code), GasLimit: 1_000_000},
		limits:   execution.DispatchLimits{PerTransaction: big.NewInt(1)},
	}
	s := &reactiveRuntime{client: node.server(t), settings: settings, updates: make(chan reactiveRefresh, 1), refreshDone: make(chan struct{}), metrics: &telemetry.Registry{}}
	ctx, cancel := context.WithCancel(t.Context())
	go s.refreshLoop(ctx)
	defer func() { cancel(); <-s.refreshDone }()
	s.updates <- reactiveRefresh{block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 100, Hash: domain.Hash{100}}, BaseFee: big.NewInt(1)}, feeParams: opFeeParams{key: "a"}}
	deadline := time.Now().Add(2 * time.Second)
	for s.cache.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("refresh not retried after the RPC served the block")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
