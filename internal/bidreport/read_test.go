package bidreport

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

type journal struct {
	t *testing.T
	bytes.Buffer
}

func (j *journal) row(value any) {
	j.t.Helper()
	if err := json.NewEncoder(&j.Buffer).Encode(value); err != nil {
		j.t.Fatal(err)
	}
}

func (j *journal) send(id byte, probability float64, at time.Time, lane string) *execution.Settlement {
	h := domain.Hash{id}
	i := &execution.ReactiveIntent{ID: h, Chain: domain.ChainIdentity{ChainID: 10, Family: domain.FamilyOPStack}, Bid: &execution.BidDecision{Applied: true, ModelTip: big.NewInt(20), Win: probability, WinRaw: 0.8, Gas: 400_000, Worth: 100_000}}
	i.Call.TipCap = big.NewInt(20)
	j.row(map[string]any{"type": "reactive_intent", "mode": "live", "transaction": h, "intent": i, "submission": map[string]any{"StartedAtNS": at.UnixNano()}, "timing": map[string]string{"lane": lane}})
	return &execution.Settlement{Hash: h, Intent: i, Basis: "canonical_receipt; admission_conversion", Fees: &domain.ReceiptFees{GasUsed: 400_000, Total: big.NewInt(10)}}
}

func (j *journal) settle(s *execution.Settlement) {
	j.row(map[string]any{"type": "settlement", "receipt": s})
}

func TestRecordedPredictionsUseCanonicalCorrectionsAndExcludeUnobservedBids(t *testing.T) {
	j := &journal{t: t}
	at := time.Date(2026, 11, 3, 7, 30, 0, 0, time.UTC)
	a := j.send(1, 0.6, at, "fast")
	a.Success, a.Basis, a.FeeStatus = true, "flashblocks_feed; admission_conversion", execution.FeesPending
	j.settle(a)
	// Canonical reverts override feed success. Repeated fees count once.
	a.Success, a.Basis, a.FeeStatus = false, "canonical_receipt; admission_conversion", ""
	j.settle(a)
	j.settle(a)
	b := j.send(2, 0.8, at.Add(time.Hour), "")
	b.Success = true
	b.Fees.GasUsed = 500_000
	j.settle(b)
	j.send(3, 0.5, at, "fast") // replacement/no inclusion cannot be labeled a loss
	c := j.send(4, 0.5, at, "fast")
	j.settle(c)
	j.row(map[string]any{"type": "settlement_voided", "transaction": c.Hash})
	j.row(map[string]any{"type": "reactive_intent", "mode": "shadow"})
	j.row(map[string]any{"type": "execution_observed"})
	r, err := Read(t.Context(), &j.Buffer, Options{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if r.Submitted != 4 || r.Scored != 2 || r.All.Success != 1 || r.Excluded["no_canonical_outcome"] != 1 || r.Excluded["voided"] != 1 {
		t.Fatalf("coverage %+v", r)
	}
	if math.Abs(r.All.Calibrated.Brier-0.2) > 1e-12 || math.Abs(r.All.Raw.Brier-0.34) > 1e-12 || r.All.SuccessGasRatioMean != 1.25 {
		t.Fatalf("scores %+v", r.All)
	}
	if r.All.Paid != 2 || r.All.RevertFees.Int64() != 10 || r.All.SuccessFees.Int64() != 10 || len(r.Lane) != 2 || len(r.Hour) != 2 {
		t.Fatalf("costs/groups %+v", r)
	}
}

func TestBidReportUsesSendTimeAndNeverRefitsPredictions(t *testing.T) {
	j := &journal{t: t}
	at := time.Date(2026, 11, 3, 7, 30, 0, 0, time.UTC)
	a := j.send(1, 0.25, at, "fast")
	a.Success = true
	j.settle(a)
	b := j.send(2, 0.9, at.Add(time.Hour), "fast")
	j.settle(b)
	// Recovered/out-of-order receipts can come later than the selected interval.
	j.settle(a)
	r, err := Read(t.Context(), &j.Buffer, Options{Limit: 100, Since: at, Until: at.Add(time.Hour)})
	if err != nil || r.Submitted != 1 || r.Scored != 1 || r.All.Calibrated.Mean != 0.25 || r.All.Calibrated.Brier != 0.5625 {
		t.Fatal(r, err)
	}
}

func TestChangedTipAndCancellationAreNotBidOutcomes(t *testing.T) {
	j := &journal{t: t}
	at := time.Unix(1800000000, 0)
	a := j.send(1, 0.5, at, "fast")
	// Change the serialized forecast, as the pool-fee repricing ladder does.
	raw := bytes.ReplaceAll(j.Bytes(), []byte(`"ModelTip":20`), []byte(`"ModelTip":30`))
	j.Reset()
	j.Write(raw)
	j.settle(a)
	b := j.send(2, 0.5, at, "fast")
	b.Cancelled = true
	j.settle(b)
	r, err := Read(t.Context(), &j.Buffer, Options{Limit: 100})
	if err != nil || r.Scored != 0 || r.Excluded["signed_tip_changed"] != 1 || r.Excluded["cancelled"] != 1 {
		t.Fatal(r, err)
	}
}

func TestBidReportRejectsInvalidEvidence(t *testing.T) {
	for _, problem := range []string{"probability", "mixed_chain", "limit", "truncated"} {
		t.Run(problem, func(t *testing.T) {
			j := &journal{t: t}
			opts := Options{Limit: 100}
			a := j.send(1, 0.5, time.Unix(1800000000, 0), "fast")
			j.settle(a)
			raw := bytes.Clone(j.Bytes())
			switch problem {
			case "probability":
				raw = bytes.ReplaceAll(raw, []byte(`"Win":0.5`), []byte(`"Win":1.5`))
			case "mixed_chain":
				j.Reset()
				j.send(2, 0.5, time.Unix(1800000000, 0), "fast")
				raw = append(raw, bytes.ReplaceAll(j.Bytes(), []byte(`"ChainID":10`), []byte(`"ChainID":8453`))...)
			case "limit":
				opts.Limit = 1
			case "truncated":
				raw = append(raw, []byte(`{"type":`)...)
			}
			if _, err := Read(t.Context(), bytes.NewReader(raw), opts); err == nil {
				t.Fatal("invalid journal accepted")
			}
		})
	}
}

func TestAppliedTreeModelHasSeparateAuditCohort(t *testing.T) {
	j := &journal{t: t}
	settled := j.send(1, .7, time.Unix(1800000000, 0), "fast")
	j.Reset()
	settled.Intent.Bid.CostVersion = "joint-v1"
	settled.Intent.Bid.Learning = &execution.BidLearning{Applied: true, ModelID: "frozen-release"}
	j.row(map[string]any{"type": "reactive_intent", "mode": "live", "transaction": settled.Hash, "intent": settled.Intent, "submission": map[string]any{"StartedAtNS": int64(1800000000000000000)}})
	j.settle(settled)
	report, err := Read(t.Context(), &j.Buffer, Options{Limit: 100})
	if err != nil || len(report.Model) != 1 || report.Model[0].Name != "joint-v1/gbdt/frozen-release" {
		t.Fatalf("missing tree model cohort: %+v %v", report.Model, err)
	}
}
