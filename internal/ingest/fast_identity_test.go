package ingest

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
)

func TestFastReceiptMetadataConflictsRetirePrefix(t *testing.T) {
	changes := map[string]func(*feed.FastMessage){
		"gas":             func(m *feed.FastMessage) { m.Receipt.GasUsed++ },
		"L1 gas":          func(m *feed.FastMessage) { m.Receipt.GasUsedForL1++ },
		"cumulative gas":  func(m *feed.FastMessage) { m.Receipt.CumulativeGasUsed++ },
		"effective price": func(m *feed.FastMessage) { m.Receipt.EffectiveGasPrice.Add(m.Receipt.EffectiveGasPrice, big.NewInt(1)) },
		"base fee":        func(m *feed.FastMessage) { m.Receipt.BaseFee.Add(m.Receipt.BaseFee, big.NewInt(1)) },
		"created address": func(m *feed.FastMessage) { m.Receipt.ContractAddress = domain.Address{9} },
		"raw transaction": func(m *feed.FastMessage) { m.RawTx[0]++ },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			branch, _, _, key := fastBranch(t, FastOptions{})
			message := fastSync(key, 11, 0, 1, 300)
			message.Receipt.EffectiveGasPrice, message.Receipt.BaseFee = big.NewInt(5), big.NewInt(4)
			if _, err := branch.Apply(t.Context(), message); err != nil {
				t.Fatal(err)
			}
			_, old, _, _ := branch.View()
			change(&message)
			if _, err := branch.Apply(t.Context(), message); !errors.Is(err, ErrFastConflict) {
				t.Fatalf("changed receipt was accepted as duplicate: %v", err)
			}
			if branch.Current(old) {
				t.Fatal("conflict left old prefix current")
			}
		})
	}
}

func TestFastPrefixIgnoresTransportTime(t *testing.T) {
	branch, _, _, key := fastBranch(t, FastOptions{})
	message := fastSync(key, 11, 0, 1, 300)
	if _, err := branch.Apply(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	_, version, _, _ := branch.View()
	message.TimestampMS++
	message.PGARound++
	if _, err := branch.Apply(t.Context(), message); err != nil || !branch.Current(version) {
		t.Fatal("transport metadata advanced execution identity")
	}
}
