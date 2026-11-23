package slipstream_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

type recordedBlock struct {
	Number     uint64 `json:"number"`
	Timestamp  uint64 `json:"timestamp"`
	Hash       string `json:"hash"`
	ParentHash string `json:"parent_hash"`
	Logs       []struct {
		Address string
		Topics  []string
		Data    string
	}
	Checks map[string]struct {
		Fee   string
		Slot0 string
	}
}

func (b recordedBlock) ref() domain.BlockRef {
	h, _ := domain.ParseHash(b.Hash)
	parent, _ := domain.ParseHash(b.ParentHash)
	return domain.BlockRef{Number: b.Number, Timestamp: b.Timestamp, Hash: h, ParentHash: parent}
}

type oracleFixture struct {
	Block      recordedBlock
	Pools      []string
	Calls      map[string]string
	CodeHashes map[string]string `json:"code_hashes"`
	Blocks     []recordedBlock
	reads      int
}

func (f *oracleFixture) FeeStateBlock() domain.BlockRef { return f.Block.ref() }
func (f *oracleFixture) CodeHash(_ context.Context, a domain.Address) (domain.Hash, error) {
	return domain.ParseHash(f.CodeHashes[a.Hex()])
}
func (f *oracleFixture) CallContract(_ context.Context, a domain.Address, data []byte, block string, out *string) error {
	if block != fmt.Sprintf("0x%x", f.Block.Number) {
		return fmt.Errorf("wrong snapshot %s", block)
	}
	v, ok := f.Calls[a.Hex()+":0x"+hex.EncodeToString(data)]
	if !ok {
		return fmt.Errorf("unrecorded call %s %x", a, data)
	}
	f.reads++
	*out = v
	return nil
}
func (f *oracleFixture) CallContracts(ctx context.Context, calls []pool.ContractCall) ([]string, error) {
	results := make([]string, len(calls))
	for i, c := range calls {
		if err := f.CallContract(ctx, c.To, c.Data, c.Block, &results[i]); err != nil {
			return nil, err
		}
	}
	return results, nil
}
func abiInt(s string, i int) *big.Int {
	n, _ := new(big.Int).SetString(strings.TrimPrefix(s, "0x")[i*64:(i+1)*64], 16)
	return n
}

// Fixture: OP Mainnet canonical blocks 158990129..158990145, captured with
// hash-pinned eth_call and ordered receipts. The oracle/configuration seed is
// independent of the later fee() checks. No RPC is used by this test.
func TestRecordedDynamicHookOracleMatchesSixteenBlocks(t *testing.T) {
	replayRecordedOracle(t, "testdata/op-dynamic-hook.json")
}

// Fixture: Arbitrum One canonical blocks 522676128..522676175 on the four most
// active Slipstream pools, recorded the same way. Arbitrum's hook is
// ArbDynamicSwapFeeHook, whose first-swap fee cache keys on the L2 block.
func TestRecordedArbitrumHookMatchesFortyEightBlocks(t *testing.T) {
	replayRecordedOracle(t, "testdata/arb-dynamic-hook.json")
}

func TestRecordedLegacyModulesMatchSixteenBlocks(t *testing.T) {
	// Two legacy deployments, canonical blocks 158990526..158990542.
	replayRecordedOracle(t, "testdata/op-dynamic-legacy.json")
}

func replayRecordedOracle(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f oracleFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	store := state.New()
	if err = store.BeginBlock(f.Block.ref()); err != nil {
		t.Fatal(err)
	}
	keys := map[string]domain.PoolKey{}
	for _, address := range f.Pools {
		a, _ := domain.ParseAddress(address)
		key := domain.PoolKey{Address: a, Kind: domain.PoolKindSlipstream}
		keys[address] = key
		p := pool.State{Key: key, Token0: domain.Address{1}, Token1: domain.Address{2}, TickSpacing: int32(abiInt(f.Calls[address+":0xd0c93a7c"], 0).Int64()), Liquidity: abiInt(f.Calls[address+":0x1a686502"], 0)}
		p, err = slipstream.LoadFee(t.Context(), &f, p, fmt.Sprintf("0x%x", f.Block.Number))
		if err != nil {
			t.Fatal(address, err)
		}
		if p.Slipstream == nil || !p.Slipstream.Valid {
			t.Fatal("recorded deployment not supported", address)
		}
		if err = store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	store.Commit()
	store.ClearDirty()
	reads := f.reads
	for _, b := range f.Blocks {
		if err = store.BeginBlock(b.ref()); err != nil {
			t.Fatal(err)
		}
		for _, log := range b.Logs {
			topics := make([]domain.Hash, len(log.Topics))
			for i, s := range log.Topics {
				topics[i], _ = domain.ParseHash(s)
			}
			data, _ := hex.DecodeString(strings.TrimPrefix(log.Data, "0x"))
			key := keys[log.Address]
			p, _ := store.Pool(key)
			matched, e := slipstream.FollowMetadata(&p, topics, data)
			if e != nil {
				t.Fatal(e)
			}
			if matched {
				if e = store.Upsert(p); e != nil {
					t.Fatal(e)
				}
				continue
			}
			ev, ok, e := uniswapv3.DecodeLog(topics, data)
			if e != nil {
				t.Fatal(e)
			}
			if !ok {
				continue
			}
			if e = store.ApplyV3(key, ev, b.Number); e != nil {
				t.Fatal(e)
			}
		}
		for a, check := range b.Checks {
			p, _ := store.Pool(keys[a])
			fee, e := slipstream.LocalFee(p)
			if e != nil || fee != uint32(abiInt(check.Fee, 0).Uint64()) {
				t.Errorf("block %d pool %s local=%d chain=%s valid=%v cached=%d err=%v", b.Number, a, fee, abiInt(check.Fee, 0), p.Slipstream.Valid, p.Slipstream.BlockFee, e)
			}
			if int16(p.Slipstream.ObservationIndex) != int16(abiInt(check.Slot0, 2).Uint64()) || p.SqrtPriceX96.Cmp(abiInt(check.Slot0, 0)) != 0 {
				t.Errorf("block %d oracle/price drift %s", b.Number, a)
			}
		}
		store.Commit()
		store.ClearDirty()
		// Restart between blocks must preserve the interpolation boundary/cache.
		encoded, e := store.EncodeSnapshot()
		if e != nil {
			t.Fatal(e)
		}
		restored := state.New()
		if e = restored.LoadSnapshot(encoded); e != nil {
			t.Fatal(e)
		}
		store = restored
	}
	if f.reads != reads {
		t.Fatal("log following issued RPC reads")
	}
}
