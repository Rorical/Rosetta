//go:build opnative && contracts

package remote

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/feed"
)

func nativeFlashPrefix(t *testing.T, f *opFixture) (flashPrefix, []map[string]any) {
	t.Helper()
	private, err := crypto.HexToECDSA("0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	sender := domain.Address(crypto.PubkeyToAddress(private.PublicKey))
	if err := f.engine.request(map[string]any{"method": "set", "accounts": map[string]any{sender.Hex(): map[string]any{"balance": "0x1000000000000000000000000"}, params.BeaconRootsAddress.Hex(): map[string]any{"code": flashHex(params.BeaconRootsCode)}, params.HistoryStorageAddress.Hex(): map[string]any{"code": flashHex(params.HistoryStorageCode)}}}, nil); err != nil {
		t.Fatal(err)
	}
	depositInput, err := f.artifacts["TestToken"].abi.Pack("mint", [20]byte(f.owner), big.NewInt(777))
	if err != nil {
		t.Fatal(err)
	}
	source := domain.Hash{44}
	depositor := addressAt(0x82)
	encoded, err := rlp.EncodeToBytes([]any{source, depositor, f.token0, uint64(1234), uint64(0), uint64(300_000), uint64(0), depositInput})
	if err != nil {
		t.Fatal(err)
	}
	raw := [][]byte{append([]byte{0x7e}, encoded...)}
	// Independent writes to token storage and a revert exercise ordered overlay
	// composition, sender fees/nonce, and the privileged deposit transition.
	for i, entry := range []struct {
		target         domain.Address
		artifact, name string
		args           []any
	}{
		{f.token0, "TestToken", "mint", []any{[20]byte(f.owner), big.NewInt(333)}},
		{f.first, "TestV3Pool", "setMode", []any{uint8(1)}},
		{f.first, "TestV3Pool", "setMode", []any{uint8(0)}},
		{f.token0, "TestToken", "transfer", []any{[20]byte(f.owner), big.NewInt(999)}},
	} {
		input, err := f.artifacts[entry.artifact].abi.Pack(entry.name, entry.args...)
		if err != nil {
			t.Fatal(err)
		}
		fields := []any{uint64(8453), uint64(i), uint64(0), uint64(2), uint64(300_000), entry.target, uint64(0), input, []any{}}
		signing, err := rlp.EncodeToBytes(fields)
		if err != nil {
			t.Fatal(err)
		}
		hash := digest(append([]byte{2}, signing...))
		sig, err := crypto.Sign(hash[:], private)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := rlp.EncodeToBytes(append(fields, uint64(sig[64]), new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:64])))
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, append([]byte{2}, signed...))
	}
	anchor := domain.BlockRef{Number: 10, Hash: domain.Hash{10}, ParentHash: domain.Hash{9}, Timestamp: 1000}
	root := domain.Hash{12}
	gas := uint64(0)
	prefix := flashPrefix{Base: feed.FlashblockBase{Parent: anchor.Hash, Number: 11, Timestamp: 1002, Execution: &feed.FlashExecution{GasLimit: 32_000_000, BaseFee: big.NewInt(1)}}, Payload: "0x0102030405060708", Index: 1, GasUsed: &gas, WithdrawalsRoot: &root, Version: domain.StateVersion{Source: domain.SourceFast, Anchor: anchor, BlockNumber: 11, TxIndex: uint64(len(raw) - 1), Epoch: 1, PrefixHash: domain.Hash{81}}, Source: domain.Hash{82}}
	if err := f.engine.request(map[string]any{"method": "checkpoint"}, nil); err != nil {
		t.Fatal(err)
	}
	defer f.engine.request(map[string]any{"method": "restore"}, nil)
	var calls []map[string]any
	for _, transaction := range raw {
		call, err := decodeNativePrefixCall(transaction)
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
		result, err := f.executeAt(call, nil, true, true, map[string]any{"number": "0xb", "time": "0x3ea"})
		if err != nil {
			t.Fatal(err)
		}
		used, err := quantity(result.GasUsed)
		if err != nil {
			t.Fatal(err)
		}
		gas += used
		if len(result.Logs) != 0 {
			t.Fatal("unexpected synthetic fixture logs")
		}
		prefix.Transactions = append(prefix.Transactions, feed.FlashTransaction{Hash: digest(transaction), Raw: transaction, Successful: result.Success})
	}
	return prefix, calls
}

func TestFlashblocksCompleteConfirmationAgainstNativeOP(t *testing.T) {
	for _, fork := range []string{"isthmus", "jovian"} {
		t.Run(fork, func(t *testing.T) {
			f := newOPFixture(t, fork)
			request := f.request(t)
			prefix, calls := nativeFlashPrefix(t, f)
			node := &flashRPC{base: &opRPC{fixture: f, replay: true}}
			client := node.server(t)
			parent, err := client.ExecutionBlock(t.Context(), "latest")
			if err != nil {
				t.Fatal(err)
			}
			view, proof, err := openFlashSnapshot(t.Context(), client, parent, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, domain.Deployments{}, prefix)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			balance, err := view.assetBalance(t.Context(), f.token0, f.owner, nil)
			if err != nil || balance.Int64() != 101110 {
				t.Fatal("prefix token writes missing", balance, err)
			}
			result, err := view.Confirm(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Profit.Int64() != 200 || result.Context.Block.Number != 11 || result.Context.Timestamp != 1002 || result.Context.Block.Hash != proof.ContextHash {
				t.Fatal("incomplete prefix confirmation", result)
			}
			if err := f.engine.request(map[string]any{"method": "checkpoint"}, nil); err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if _, err := f.executeAt(call, nil, true, true, map[string]any{"number": "0xb", "time": "0x3ea"}); err != nil {
					t.Fatal(err)
				}
			}
			invocation, err := execution.BuildExecutorCall(t.Context(), request.Plan, request.Parameters)
			if err != nil {
				t.Fatal(err)
			}
			args := executorArgs(invocation, 8453)
			envelope, err := execution.UnsignedDynamicFeeTx(invocation, 8453)
			if err != nil {
				t.Fatal(err)
			}
			args["envelope"] = flashHex(envelope)
			reference, err := f.executeAt(args, nil, false, true, map[string]any{"number": "0xb", "time": "0x3ea"})
			if err != nil || !reference.Success || reference.GasUsed != fmtGas(result.GasUsed) {
				t.Fatal("prefix reference execution differs", reference, err)
			}
			if err := f.engine.request(map[string]any{"method": "restore"}, nil); err != nil {
				t.Fatal(err)
			}
			canonical := &snapshot{client: client, block: parent, chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}}
			untouched, err := canonical.assetBalance(t.Context(), f.token0, f.owner, nil)
			if err != nil || untouched.Int64() != 100000 {
				t.Fatal("private prefix mutated canonical state", untouched, err)
			}
			for _, fault := range []string{"count", "hash", "status", "diff", "logs"} {
				node.fault = fault
				if _, _, err := openFlashSnapshot(t.Context(), client, parent, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, domain.Deployments{}, prefix); err == nil {
					t.Fatal("invalid replay accepted", fault)
				}
			}
			node.fault = ""
			node.base.fault.Store(3)
			if _, _, err := openFlashSnapshot(t.Context(), client, parent, domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}, domain.Deployments{}, prefix); err == nil {
				t.Fatal("reorganized anchor accepted")
			}
		})
	}
}
func fmtGas(value uint64) string { return "0x" + new(big.Int).SetUint64(value).Text(16) }
