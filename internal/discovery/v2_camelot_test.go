package discovery

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func pairLog(source Source, address domain.Address) rpc.Log {
	data := testAddressWord(address)
	if source == SourceUniswapV2 {
		data = append(data, testUintWord(1)...)
	}
	return rpc.Log{Address: source.address(), Topics: []domain.Hash{source.topic0(), addressTopic(repeatedAddress(1)), addressTopic(repeatedAddress(2))}, Data: data}
}

func TestV2CamelotFactoryDiscoveryAndSnapshotMetadata(t *testing.T) {
	logs := newFakeLogs()
	caller := newFakeCaller()
	v2, camelot, expensive := repeatedAddress(0x41), repeatedAddress(0x42), repeatedAddress(0x43)
	logs.add(pairLog(SourceUniswapV2, v2), pairLog(SourceCamelotV3, camelot), pairLog(SourceCamelotV3, expensive))
	caller.set(v2, []byte{9, 2, 0xf1, 0xac}, "0x"+hex.EncodeToString(concatWords(testUintWord(100), testUintWord(400), testUintWord(0))))
	for _, address := range []domain.Address{camelot, expensive} {
		reverse := uint64(3000)
		if address == expensive {
			reverse = 40000
		}
		global := concatWords(testUintWord(1), testUintWord(0), testUintWord(500), testUintWord(reverse), testUintWord(0), testUintWord(0), testUintWord(0), testUintWord(1))
		caller.set(address, camelotGlobalCall, "0x"+hex.EncodeToString(global))
		caller.set(address, camelotSpacingCall, wordHex(big.NewInt(60)))
		caller.setLiquidity(Spec{Address: address, Kind: domain.PoolKindCamelotV3}, big.NewInt(300))
	}
	d, err := New(logs, caller, testConfig([]Source{SourceUniswapV2, SourceCamelotV3}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := d.Discover(t.Context(), 100, 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pools) != 2 || result.Pools[0].Address != camelot || result.Pools[1].Address != v2 || result.Stats.SkippedFee != 1 {
		t.Fatalf("unexpected discovery: %+v", result)
	}
	if result.Pools[0].TickSpacing != 60 || result.Pools[0].Fee != 500 || result.Pools[1].Fee != 3000 {
		t.Fatal("metadata/default fee not applied")
	}
	for _, call := range caller.calls {
		if call.Block != "0x65" {
			t.Fatal("metadata calls mixed snapshots")
		}
	}
}

func TestNewFactoryEventShapesAndEmitterIdentity(t *testing.T) {
	for _, source := range []Source{SourceUniswapV2, SourceCamelotV3} {
		log := pairLog(source, repeatedAddress(0x44))
		spec, ok, err := decodeSpec(source, log)
		if err != nil || !ok || spec.Address != repeatedAddress(0x44) {
			t.Fatal("official event shape not decoded")
		}
		if !source.Matches(log.Address, log.Topics) || source.Matches(repeatedAddress(9), log.Topics) {
			t.Fatal("factory emitter not enforced")
		}
		log.Data[0] = 1
		if _, _, err := decodeSpec(source, log); err == nil {
			t.Fatal("malformed address padding accepted")
		}
		log = pairLog(source, repeatedAddress(0x44))
		log.Topics = log.Topics[:2]
		if _, _, err := decodeSpec(source, log); err == nil {
			t.Fatal("truncated topics accepted")
		}
	}
	if SourceUniswapV2.address() != mustAddress(t, "0xf1D7CC64Fb4452F05c498126312eBE29f30Fbcf9") || SourceCamelotV3.address() != mustAddress(t, "0x1a3c9B1d2F0529D97f2afC5136Cc23e58f1FD35B") {
		t.Fatal("factory address transcription")
	}
	if topicV2PairCreated != mustHash(t, "0x0d3648bd0f6ba80134a33ba9275ac585d9d315f0ad8355cddefde31afa28d0e9") || topicCamelotPool != mustHash(t, "0x91ccaa7a278130b65168c3a0c8d3bcae84cf5e43704342bd3ec0b59e59c036db") {
		t.Fatal("creation signature mismatch")
	}
}
