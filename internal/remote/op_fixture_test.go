//go:build opnative && contracts

package remote

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

type opArtifact struct {
	abi  abi.ABI
	code []byte
}
type opFixture struct {
	engine                                                 *opEngine
	artifacts                                              map[string]opArtifact
	owner, executor, token0, token1, first, second, lender domain.Address
	nonce                                                  uint64
	fork                                                   string
}
type opResult struct {
	Output, GasUsed string
	Logs            []map[string]any
	Success         bool
	Created         *string
	Pre, Post       map[string]map[string]any
}

func newOPFixture(t *testing.T, fork string) *opFixture {
	t.Helper()
	f := &opFixture{engine: newOPEngine(t), owner: addressAt(0x81), artifacts: map[string]opArtifact{}, fork: fork}
	output := t.TempDir()
	// Compile the executor and mocks; Foundry runs .t.sol suites separately.
	build := exec.CommandContext(t.Context(), "forge", "build", "--offline", "--skip", "test", "--out", output, "--cache-path", filepath.Join(output, "cache"))
	build.Dir = "../../contracts"
	if log, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile executor: %v\n%s", err, log)
	}
	for name, source := range map[string]string{"ArbitrageExecutor": "ArbitrageExecutor.sol", "TestToken": "PoolMocks.sol", "TestV3Pool": "PoolMocks.sol", "TestV4Manager": "V4Mock.sol", "TestRoundFeed": "RoundFeedMock.sol"} {
		raw, err := os.ReadFile(filepath.Join(output, source, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var artifact struct {
			ABI      json.RawMessage
			Bytecode struct{ Object string }
		}
		if err := json.Unmarshal(raw, &artifact); err != nil {
			t.Fatal(err)
		}
		parsed, err := abi.JSON(strings.NewReader(string(artifact.ABI)))
		if err != nil {
			t.Fatal(err)
		}
		code, err := hexData(artifact.Bytecode.Object)
		if err != nil {
			t.Fatal(err)
		}
		f.artifacts[name] = opArtifact{abi: parsed, code: code}
	}
	balance := "0x" + new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil).Text(16)
	if err := f.engine.request(map[string]any{"method": "set", "accounts": map[string]any{f.owner.Hex(): map[string]any{"balance": balance}}}, nil); err != nil {
		t.Fatal(err)
	}
	a, b := f.deploy(t, "TestToken"), f.deploy(t, "TestToken")
	if a.Hex() < b.Hex() {
		f.token0, f.token1 = a, b
	} else {
		f.token0, f.token1 = b, a
	}
	f.executor = f.deploy(t, "ArbitrageExecutor", [20]byte(f.owner), [2][20]byte{})
	f.first = f.deploy(t, "TestV3Pool", [20]byte(f.token0), [20]byte(f.token1), big.NewInt(2_000_000), uint8(0))
	f.second = f.deploy(t, "TestV3Pool", [20]byte(f.token0), [20]byte(f.token1), big.NewInt(600_000), uint8(0))
	f.lender = f.deploy(t, "TestV3Pool", [20]byte(f.token0), [20]byte(f.token1), big.NewInt(1_000_000), uint8(0))
	for _, target := range []domain.Address{f.first, f.second, f.lender} {
		for _, token := range []domain.Address{f.token0, f.token1} {
			f.call(t, "TestToken", token, "mint", [20]byte(target), big.NewInt(10_000_000))
		}
	}
	f.call(t, "TestToken", f.token0, "mint", [20]byte(f.owner), big.NewInt(100_000))
	f.call(t, "TestToken", f.token0, "approve", [20]byte(f.executor), new(big.Int).SetUint64(^uint64(0)))
	f.call(t, "ArbitrageExecutor", f.executor, "setPaused", false)
	return f
}
func (f *opFixture) execute(call map[string]any, overrides any, commit, native bool) (opResult, error) {
	return f.executeAt(call, overrides, commit, native, nil)
}
func (f *opFixture) executeAt(call map[string]any, overrides any, commit, native bool, block map[string]any) (opResult, error) {
	input := map[string]any{"method": "execute", "call": call, "overrides": overrides, "commit": commit, "native_fees": native, "fork": f.fork, "envelope": call["envelope"]}
	for field, key := range map[string]string{"number": "number", "time": "timestamp", "gasLimit": "gas_limit", "baseFeePerGas": "base_fee"} {
		if text, ok := block[field].(string); ok {
			value, err := quantity(text)
			if err != nil {
				return opResult{}, err
			}
			input[key] = value
		}
	}
	var result opResult
	err := f.engine.request(input, &result)
	return result, err
}

func (f *opFixture) transact(t *testing.T, to *domain.Address, data []byte) opResult {
	t.Helper()
	call := map[string]any{"from": f.owner.Hex(), "data": "0x" + hex.EncodeToString(data), "gas": "0x800000", "maxFeePerGas": "0x2", "maxPriorityFeePerGas": "0x0"}
	if to != nil {
		call["to"] = to.Hex()
	}
	result, err := f.execute(call, nil, true, false)
	if err != nil || !result.Success {
		t.Fatalf("OP fixture transaction: %v %s", err, result.Output)
	}
	f.nonce++
	return result
}
func (f *opFixture) deploy(t *testing.T, name string, args ...any) domain.Address {
	t.Helper()
	artifact := f.artifacts[name]
	data, err := artifact.abi.Pack("", args...)
	if err != nil {
		t.Fatal(err)
	}
	result := f.transact(t, nil, append(append([]byte(nil), artifact.code...), data...))
	if result.Created == nil {
		t.Fatal("OP creation did not return address")
	}
	address, err := domain.ParseAddress(*result.Created)
	if err != nil {
		t.Fatal(err)
	}
	return address
}
func (f *opFixture) call(t *testing.T, name string, to domain.Address, method string, args ...any) {
	t.Helper()
	data, err := f.artifacts[name].abi.Pack(method, args...)
	if err != nil {
		t.Fatal(err)
	}
	f.transact(t, &to, data)
}
func (f *opFixture) request(t *testing.T) execution.ConfirmationRequest {
	t.Helper()
	var account struct{ Code string }
	if err := f.engine.request(map[string]any{"method": "account", "address": f.executor.Hex()}, &account); err != nil {
		t.Fatal(err)
	}
	code, err := hexData(account.Code)
	if err != nil {
		t.Fatal(err)
	}
	return execution.ConfirmationRequest{Parameters: execution.CallParameters{Sender: f.owner, Executor: f.executor, CodeHash: digest(code), Nonce: f.nonce, GasLimit: 2_000_000, FeeCap: big.NewInt(2), TipCap: new(big.Int)}, Plan: execution.ExecutorPlan{Asset: f.token0, Amount: big.NewInt(1000), MaxLoanFee: new(big.Int), MinProfit: big.NewInt(100), Deadline: 1030, Steps: []execution.ExecutorStep{
		{Pool: domain.PoolKey{Kind: domain.PoolKindV3, Address: f.first}, TokenIn: f.token0, TokenOut: f.token1, Fee: 3000, TickSpacing: 60, MinAmountOut: big.NewInt(1900)},
		{Pool: domain.PoolKey{Kind: domain.PoolKindV3, Address: f.second}, TokenIn: f.token1, TokenOut: f.token0, Fee: 3000, TickSpacing: 60, MinAmountOut: big.NewInt(1100)}}}}
}

func (f *opFixture) nativePool(t *testing.T, rate int64) domain.PoolKey {
	manager := f.deploy(t, "TestV4Manager", big.NewInt(rate))
	if err := f.engine.request(map[string]any{"method": "set", "accounts": map[string]any{manager.Hex(): map[string]any{"balance": "0x100000000000000000000"}}}, nil); err != nil {
		t.Fatal(err)
	}
	f.call(t, "TestToken", f.token1, "mint", [20]byte(manager), big.NewInt(1_000_000))
	key := make([]byte, 160)
	copy(key[44:64], f.token1[:])
	big.NewInt(3000).FillBytes(key[64:96])
	big.NewInt(60).FillBytes(key[96:128])
	return domain.PoolKey{Kind: domain.PoolKindV4, Address: manager, PoolID: digest(key)}
}
