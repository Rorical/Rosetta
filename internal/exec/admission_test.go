package exec

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
	"github.com/ethereum/go-ethereum/core/types"
)

type admissionSigner struct {
	TransactionSigner
	calls int
	after func()
}

func (s *admissionSigner) Sign(ctx context.Context, tx *types.Transaction) (*types.Transaction, error) {
	s.calls++
	signed, err := s.TransactionSigner.Sign(ctx, tx)
	if s.after != nil {
		s.after()
	}
	return signed, err
}

func TestLiveDispatchRechecksTransferRiskBeforeAndAfterSigning(t *testing.T) {
	for _, duringSign := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "during_sign"}[duringSign], func(t *testing.T) {
			i, l, key := dispatchFixture(t)
			risks, err := tokenrisk.Open("", i.Chain)
			if err != nil {
				t.Fatal(err)
			}
			defer risks.Close()
			l.TokenCheck = risks.Check
			mark := func() {
				t.Helper()
				if err := risks.Record(tokenrisk.Evidence{Token: i.Route.Steps[1].TokenIn, Requested: big.NewInt(1000), Received: big.NewInt(990), Block: domain.BlockRef{Hash: domain.Hash{1}}, Source: "executor_trace"}); err != nil {
					t.Fatal(err)
				}
			}
			signer := &admissionSigner{TransactionSigner: key}
			if duringSign {
				signer.after = mark
			} else {
				mark()
			}
			network := &dispatchNetworkFixture{}
			d, err := OpenDispatcher(t.Context(), filepath.Join(t.TempDir(), "dispatch.json"), l, signer, network)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err = d.Dispatch(t.Context(), i, func() bool { return true }); !errors.Is(err, ErrDispatchRejected) {
				t.Fatal(err)
			}
			if network.calls != 0 || !duringSign && signer.calls != 0 {
				t.Fatal("excluded token reached signing or broadcast")
			}
		})
	}
}

func TestProfitGateRejectsCostsThatConsumeGross(t *testing.T) {
	for _, test := range []struct {
		gross, reserve, loan, fee, minimum int64
		ok                                 bool
	}{
		{100, 0, 10, 89, 1, true}, {100, 0, 10, 90, 1, false}, {100, 5, 10, 85, 1, false}, {100, 0, 0, 101, 1, false},
	} {
		_, err := CheckQuotedProfit(big.NewInt(test.gross), big.NewInt(test.reserve), big.NewInt(test.loan), big.NewInt(test.fee), big.NewInt(test.minimum))
		if (err == nil) != test.ok {
			t.Fatalf("%+v: %v", test, err)
		}
	}
	if _, err := CheckQuotedProfit(big.NewInt(100), new(big.Int), new(big.Int), nil, big.NewInt(1)); err == nil {
		t.Fatal("unknown fee accepted")
	}
	i, _, _ := dispatchFixture(t)
	i.QuotedGross = new(big.Int).Set(i.Budget)
	i.ExpectedNet = big.NewInt(1)
	if _, err := SealReactiveIntent(t.Context(), i); err == nil {
		t.Fatal("inflated expected net reached live admission")
	}
}
