package discovery

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestProbeIdentifiesVelodromeV1PairsWithoutFactoryGetter(t *testing.T) {
	legacy := velodromev2.LegacyFactory()
	a, b, pair := repeatedAddress(1), repeatedAddress(2), repeatedAddress(4)
	caller := newFakeCaller()
	caller.setError(pair, probeSelector("factory()"), fmt.Errorf("%w: execution reverted", pool.ErrCallRejected))
	words := [][]byte{big.NewInt(1e18).FillBytes(make([]byte, 32)), big.NewInt(1e6).FillBytes(make([]byte, 32)),
		big.NewInt(5e18).FillBytes(make([]byte, 32)), big.NewInt(9e9).FillBytes(make([]byte, 32)), big.NewInt(1).FillBytes(make([]byte, 32)),
		testAddressWord(a), testAddressWord(b)}
	var metadata []byte
	for _, w := range words {
		metadata = append(metadata, w...)
	}
	caller.set(pair, probeSelector("metadata()"), "0x"+hex.EncodeToString(metadata))
	lookup := append(append(append(probeSelector("getPair(address,address,bool)"), testAddressWord(a)...), testAddressWord(b)...), big.NewInt(1).FillBytes(make([]byte, 32))...)
	caller.set(legacy, lookup, "0x"+hex.EncodeToString(testAddressWord(pair)))
	fee := append([]byte{0x51, 0x2b, 0x45, 0xea}, big.NewInt(1).FillBytes(make([]byte, 32))...)
	caller.set(legacy, fee, wordHex(big.NewInt(5)))
	key := domain.PoolKey{Address: pair}
	options := ProbeOptions{Emitters: []Emitter{{SourceVelodromeV1, legacy}}, Activity: map[domain.PoolKey]Activity{key: {Swaps: 9}}, Seen: map[domain.PoolKey]bool{}, Anchor: domain.BlockRef{Number: 10}, Limit: 2, Cooldown: 100}
	probe := ActiveProbe{}
	rows, err := probe.Run(t.Context(), caller, options)
	if err != nil || len(rows) != 1 {
		t.Fatal("V1 pair not identified", rows, err)
	}
	spec := rows[0].Spec
	if rows[0].Source != SourceVelodromeV1 || spec.Kind != domain.PoolKindVelodromeV2 || spec.Factory != legacy || !spec.Stable || spec.Fee != 500 || spec.Token0 != a || spec.Token1 != b {
		t.Fatalf("V1 pair identified as %+v from %s", spec, rows[0].Source)
	}
	// A pair the V1 factory does not return is not admitted.
	caller.set(legacy, lookup, "0x"+hex.EncodeToString(testAddressWord(repeatedAddress(9))))
	options.Anchor.Number += 200
	probe = ActiveProbe{}
	if rows, err := probe.Run(t.Context(), caller, options); err != nil || len(rows) != 0 {
		t.Fatal("unregistered V1-shaped pair admitted", rows, err)
	}
}

func TestDecodeVelodromeV1PairCreatedReadsStableFromData(t *testing.T) {
	a, b, pair := repeatedAddress(1), repeatedAddress(2), repeatedAddress(4)
	data := append(append(big.NewInt(1).FillBytes(make([]byte, 32)), testAddressWord(pair)...), big.NewInt(677).FillBytes(make([]byte, 32))...)
	log := rpc.Log{Address: velodromev2.LegacyFactory(), Topics: []domain.Hash{topicVelodromeV1PairCreated, domain.Hash(testAddressWord(a)), domain.Hash(testAddressWord(b))}, Data: data}
	spec, ok, err := DecodeVelodromeV1PairCreated(log)
	if err != nil || !ok || spec.Address != pair || !spec.Stable || spec.Factory != velodromev2.LegacyFactory() || spec.Kind != domain.PoolKindVelodromeV2 {
		t.Fatal(spec, ok, err)
	}
	if _, _, err := decodeSpec(SourceVelodromeV1, log); err != nil {
		t.Fatal("scan dispatch lacks the V1 decoder", err)
	}
}
