package discovery

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func mustAddress(t *testing.T, text string) domain.Address {
	t.Helper()
	address, err := domain.ParseAddress(text)
	if err != nil {
		t.Fatalf("ParseAddress(%q): %v", text, err)
	}
	return address
}

func mustHash(t *testing.T, text string) domain.Hash {
	t.Helper()
	hash, err := domain.ParseHash(text)
	if err != nil {
		t.Fatalf("ParseHash(%q): %v", text, err)
	}
	return hash
}

// repeatedAddress builds a distinct address per tag.
func repeatedAddress(tag byte) domain.Address {
	var address domain.Address
	for i := range address {
		address[i] = tag
	}
	return address
}

func testAddressWord(address domain.Address) []byte {
	word := make([]byte, abiWordSize)
	copy(word[addressPadding:], address[:])
	return word
}

func testUtestInt24Word(value uint32) []byte {
	word := make([]byte, abiWordSize)
	word[29] = byte(value >> 16)
	word[30] = byte(value >> 8)
	word[31] = byte(value)
	return word
}

func testInt24Word(value int32) []byte {
	word := make([]byte, abiWordSize)
	if value < 0 {
		for i := 0; i < abiWordSize-3; i++ {
			word[i] = 0xff
		}
	}
	raw := uint32(value) & 0xffffff
	word[29] = byte(raw >> 16)
	word[30] = byte(raw >> 8)
	word[31] = byte(raw)
	return word
}

func testUintWord(value uint64) []byte {
	var word [abiWordSize]byte
	big.NewInt(int64(value)).FillBytes(word[:])
	return word[:]
}

func concatWords(words ...[]byte) []byte {
	out := make([]byte, 0, len(words)*abiWordSize)
	for _, word := range words {
		out = append(out, word...)
	}
	return out
}

func hashFromWord(word []byte) domain.Hash {
	var hash domain.Hash
	copy(hash[:], word)
	return hash
}

func addressTopic(address domain.Address) domain.Hash {
	return hashFromWord(testAddressWord(address))
}

func v3Log(token0, token1 domain.Address, fee uint32, tickSpacing int32, poolAddress domain.Address) rpc.Log {
	return rpc.Log{
		Address: uniswapV3Factory,
		Topics: []domain.Hash{
			topicV3PoolCreated,
			addressTopic(token0),
			addressTopic(token1),
			hashFromWord(testUtestInt24Word(fee)),
		},
		Data: concatWords(testInt24Word(tickSpacing), testAddressWord(poolAddress)),
	}
}

func slipstreamLog(token0, token1 domain.Address, tickSpacing int32, poolAddress domain.Address) rpc.Log {
	return rpc.Log{
		Address: slipstreamFactory,
		Topics: []domain.Hash{
			topicSlipstreamPoolCreated,
			addressTopic(token0),
			addressTopic(token1),
			hashFromWord(testInt24Word(tickSpacing)),
		},
		Data: testAddressWord(poolAddress),
	}
}

func v4Log(t *testing.T, token0, token1 domain.Address, fee uint32, tickSpacing int32, hooks domain.Address) rpc.Log {
	t.Helper()
	id, err := uniswapv4.PoolID(token0, token1, fee, tickSpacing, hooks)
	if err != nil {
		t.Fatalf("PoolID: %v", err)
	}
	return v4LogWithID(token0, token1, fee, tickSpacing, hooks, id)
}

func v4LogWithID(token0, token1 domain.Address, fee uint32, tickSpacing int32, hooks domain.Address, id domain.Hash) rpc.Log {
	return rpc.Log{
		Address: v4PoolManager,
		Topics: []domain.Hash{
			topicV4Initialize,
			id,
			addressTopic(token0),
			addressTopic(token1),
		},
		Data: concatWords(
			testUtestInt24Word(fee),
			testInt24Word(tickSpacing),
			testAddressWord(hooks),
			testUintWord(1), // sqrtPriceX96
			testInt24Word(0),
		),
	}
}

func wordHex(value *big.Int) string {
	var word [abiWordSize]byte
	value.FillBytes(word[:])
	return "0x" + hex.EncodeToString(word[:])
}

func callerKey(to domain.Address, data []byte) string {
	return to.Hex() + ":" + hex.EncodeToString(data)
}

type fakeCaller struct {
	outputs map[string]string
	errs    map[string]error
	calls   []pool.ContractCall
}

func newFakeCaller() *fakeCaller {
	return &fakeCaller{outputs: make(map[string]string), errs: make(map[string]error)}
}

func (f *fakeCaller) set(to domain.Address, data []byte, output string) {
	f.outputs[callerKey(to, data)] = output
}

func (f *fakeCaller) setError(to domain.Address, data []byte, err error) {
	f.errs[callerKey(to, data)] = err
}

func (f *fakeCaller) setLiquidity(spec Spec, value *big.Int) {
	f.set(liquidityTarget(spec), liquidityCallData(spec), wordHex(value))
}

func (f *fakeCaller) setFee(poolAddress domain.Address, fee uint32) {
	f.set(poolAddress, feeCall, wordHex(big.NewInt(int64(fee))))
}

func (f *fakeCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.calls = append(f.calls, pool.ContractCall{To: to, Data: append([]byte(nil), data...), Block: block})
	key := callerKey(to, data)
	if err, ok := f.errs[key]; ok {
		return err
	}
	output, ok := f.outputs[key]
	if !ok {
		return fmt.Errorf("fake caller: unexpected call %s", key)
	}
	*out = output
	return nil
}

// fakeBatchCaller records each round as one batch, then delegates to the
// per-call fake so fixtures are shared.
type fakeBatchCaller struct {
	*fakeCaller
	batches [][]pool.ContractCall
}

func (f *fakeBatchCaller) CallContracts(ctx context.Context, calls []pool.ContractCall) ([]string, error) {
	f.batches = append(f.batches, calls)
	outputs := make([]string, len(calls))
	for i, call := range calls {
		if err := f.CallContract(ctx, call.To, call.Data, call.Block, &outputs[i]); err != nil {
			return nil, err
		}
	}
	return outputs, nil
}

type fakeLogs struct {
	logs     map[domain.Address][]rpc.Log
	filters  []rpc.LogFilter
	err      error
	maxRange uint64 // a wider filter fails like a node timeout; zero means none
}

func newFakeLogs() *fakeLogs {
	return &fakeLogs{logs: make(map[domain.Address][]rpc.Log)}
}

func (f *fakeLogs) add(logs ...rpc.Log) {
	for _, log := range logs {
		f.logs[log.Address] = append(f.logs[log.Address], log)
	}
}

func (f *fakeLogs) GetLogs(ctx context.Context, filter rpc.LogFilter) ([]rpc.Log, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.filters = append(f.filters, filter)
	if f.err != nil {
		return nil, f.err
	}
	if f.maxRange > 0 && filter.ToBlock-filter.FromBlock+1 > f.maxRange {
		return nil, &rpc.RPCError{Code: -32002, Message: "request timed out"}
	}
	var out []rpc.Log
	for _, address := range filter.Addresses {
		for _, log := range f.logs[address] {
			if log.BlockNumber != 0 && (log.BlockNumber < filter.FromBlock || log.BlockNumber > filter.ToBlock) {
				continue
			}
			out = append(out, log)
		}
	}
	return out, nil
}

func testConfig(sources []Source, quotes ...domain.Address) Config {
	return Config{
		LookbackBlocks:  defaultLookbackBlocks,
		RefreshBlocks:   defaultRefreshBlocks,
		ScanChunkBlocks: defaultScanChunkBlocks,
		MaxPools:        defaultMaxPools,
		MinLiquidity:    new(big.Int),
		MaxFee:          defaultMaxFee,
		QuoteTokens:     quotes,
		Sources:         sources,
	}
}
