package tokenrisk

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"math/big"
	"path/filepath"
	"testing"
)

func TestTransferShortfallSurvivesRestartAndRejectsOtherChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "risks.jsonl")
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	r, err := Open(path, chain)
	if err != nil {
		t.Fatal(err)
	}
	token := domain.Address{1}
	e := Evidence{Token: token, Requested: big.NewInt(1000), Received: big.NewInt(990), Block: domain.BlockRef{Number: 12, Hash: domain.Hash{2}}, Source: "executor_trace"}
	if err = r.Record(e); err != nil {
		t.Fatal(err)
	}
	if r.Check(token) == nil || r.Check(domain.Address{2}) != nil {
		t.Fatal("risk filter")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(path, chain)
	if err != nil {
		t.Fatal(err)
	}
	if r.Check(token) == nil {
		t.Fatal("lost persisted exclusion")
	}
	r.Close()
	chain.ChainID = 42161
	if r, err = Open(path, chain); err == nil {
		r.Close()
		t.Fatal("accepted another chain's risk evidence")
	}
}

func TestEqualOrAmbiguousBalancesNeverBecomeTaxEvidence(t *testing.T) {
	r, err := Open("", domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, received := range []int64{-1, 100, 101} {
		if r.Record(Evidence{Token: domain.Address{1}, Requested: big.NewInt(100), Received: big.NewInt(received), Block: domain.BlockRef{Hash: domain.Hash{1}}, Source: "executor_trace"}) == nil {
			t.Fatal("accepted invalid shortfall")
		}
	}
}

// Receipt evidence of a taxed transfer is recorded like executor trace
// evidence and survives a reload; an unknown source is not.
func TestReceiptTransferEvidenceIsRecorded(t *testing.T) {
	path := t.TempDir() + "/risks.jsonl"
	chain := domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}
	r, err := Open(path, chain)
	if err != nil {
		t.Fatal(err)
	}
	token := domain.Address{0x58}
	e := Evidence{Token: token, Requested: big.NewInt(145), Received: big.NewInt(139), Block: domain.BlockRef{Number: 1, Hash: domain.Hash{1}}, Source: SourceReceiptTransfer}
	if err := r.Record(e); err != nil {
		t.Fatal(err)
	}
	e.Token, e.Source = domain.Address{0x59}, "rumor"
	if r.Record(e) == nil {
		t.Fatal("evidence of an unknown source was recorded")
	}
	r.Close()
	reopened, err := Open(path, chain)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Check(token) == nil {
		t.Fatal("receipt evidence was lost on reload")
	}
}
