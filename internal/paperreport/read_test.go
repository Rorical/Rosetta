package paperreport

import (
	"bytes"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func testSample(t *testing.T, id byte, delay int64, asset byte, status, netStatus string, net int64) sample {
	s := sample{Type: "execution", Schema: 1, Basis: "executor_simulation", Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Artifact: domain.Hash{9}, ID: domain.Hash{id}, Group: domain.Hash{1}, DelayMS: delay, Status: status, StateBasis: "discovery_snapshot", Call: json.RawMessage(`{"Data":"same","Nonce":7}`), Version: json.RawMessage(`{"Epoch":1}`), DiscoveredAtNS: 1000000000, ObservedAtNS: 1000000000, StartedAtNS: 1000000100, FinishedAtNS: 1000000200, GasUsed: 12345, QuotedGross: big.NewInt(30), Profit: big.NewInt(25), LoanFee: big.NewInt(2)}
	if err := json.Unmarshal([]byte(`{"candidate":{"Cycle":{"Start":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]}},"economics":{"basis":"executor_simulation","net_status":"unknown"}}`), &s); err != nil {
		t.Fatal(err)
	}
	s.Candidate.Cycle.Start = domain.Address{asset}
	s.Economics.Status, s.Economics.Net = netStatus, big.NewInt(net)
	if delay > 0 {
		s.StateBasis = "later_observed_prefix"
		s.ActualDelayNS = delay*1000000 + 50000000
		s.ObservedAtNS += s.ActualDelayNS
		s.StartedAtNS = s.ObservedAtNS + 200
		s.FinishedAtNS = s.StartedAtNS + 400
	}
	s.QueuedAtNS = s.ObservedAtNS
	return s
}
func journal(t *testing.T, records ...sample) []byte {
	t.Helper()
	var buffer bytes.Buffer
	header := sample{Type: "session", Schema: 1, Basis: "executor_simulation", Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Artifact: domain.Hash{9}}
	encoder := json.NewEncoder(&buffer)
	if err := encoder.Encode(header); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	return buffer.Bytes()
}

func TestReportExcludesLegacyMissingRPCMeasurement(t *testing.T) {
	legacy := testSample(t, 1, 0, 1, "success", "eligible", 20)
	zero, measured := uint64(0), uint64(17)
	legacy.RPCCalls = &zero
	current := testSample(t, 2, 0, 1, "success", "eligible", 20)
	current.Group = domain.Hash{2}
	current.RPCCalls = &measured
	report, err := Read(t.Context(), bytes.NewReader(journal(t, legacy, current)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Buckets[0].RPCCalls; got.Count != 1 || got.P50 != 17 {
		t.Fatal("missing measurement counted as zero RPC", got)
	}
}

func TestReportRetainsFundingRejectionWithoutExecutionEconomics(t *testing.T) {
	rejected := testSample(t, 1, 0, 1, "rejected_funding", "unknown", 0)
	rejected.StateBasis = "unverified"
	rejected.Call, rejected.Profit, rejected.LoanFee = nil, nil, nil
	rejected.GasUsed = 0
	rejected.Economics.Net = nil

	report, err := Read(t.Context(), bytes.NewReader(journal(t, rejected)), 10)
	if err != nil {
		t.Fatal(err)
	}
	bucket := report.Buckets[0]
	if report.Samples != 1 || bucket.Status["rejected_funding"] != 1 || bucket.NetStatus["unknown"] != 1 {
		t.Fatal("funding rejection missing from report", report)
	}
	if bucket.Verified != 0 || bucket.KnownNet != 0 || bucket.NetSum.Sign() != 0 || bucket.Gas.Count != 0 || len(report.Bids) != 0 {
		t.Fatal("funding rejection counted as an executed transaction", report)
	}
}

func TestAdmissionRejectionsAreNotExecutedLosses(t *testing.T) {
	for _, status := range []string{"rejected_fees", "rejected_fee_unknown", "rejected_token"} {
		rejected := testSample(t, 1, 0, 1, status, "non_positive", -100)
		rejected.Economics.Basis = "quoted_fee_preflight"
		rejected.Call, rejected.Profit, rejected.LoanFee = nil, nil, nil
		rejected.GasUsed = 0
		report, err := Read(t.Context(), bytes.NewReader(journal(t, rejected)), 10)
		if err != nil {
			t.Fatal(err)
		}
		bucket := report.Buckets[0]
		if bucket.Status[status] != 1 || bucket.Verified != 0 || bucket.KnownNet != 0 || bucket.NetSum.Sign() != 0 {
			t.Fatal("admission estimate became executed loss", bucket)
		}
	}
}

func TestReportPairsFailuresUnknownAndTokenIsolation(t *testing.T) {
	baseline := testSample(t, 1, 0, 1, "success", "eligible", 20)
	lost := testSample(t, 2, 200, 1, "reverted", "non_positive", -5)
	unknown := testSample(t, 3, 600, 1, "success", "unknown", -1000)
	other := testSample(t, 4, 0, 2, "success", "eligible", 999999)
	other.Group = domain.Hash{4}
	raw := journal(t, baseline, lost, unknown, other, lost)
	raw = append(raw, []byte(`{"unfinished":`)...)
	result, err := Read(t.Context(), bytes.NewReader(raw), 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Samples != 4 || result.Duplicates != 1 || !result.TornTail || len(result.Buckets) != 4 {
		t.Fatal(result)
	}
	first, second := result.Buckets[0], result.Buckets[1]
	if first.NetSum.Int64() != 20 || second.NetSum.Int64() != 999999 || first.GrossErrorSum.Int64() != -3 {
		t.Fatal("currency or gross error mixed", result)
	}
	delayed := result.Buckets[2]
	if delayed.Paired.PositiveBaseline != 1 || delayed.Paired.Reverted != 1 || delayed.NetSum.Int64() != -5 || delayed.Gas.P95 != 12345 || delayed.ActualDelay.P50 != 250000000 || delayed.Queue.P50 != 200 {
		t.Fatal(delayed)
	}
	missingNet := result.Buckets[3]
	if missingNet.KnownNet != 0 || missingNet.NetSum.Sign() != 0 || missingNet.NetStatus["unknown"] != 1 || missingNet.Paired.Unknown != 1 {
		t.Fatal("partial negative estimate counted as a loss", missingNet)
	}
}

func TestReportRejectsConflictingIdentityLimitsAndCorruption(t *testing.T) {
	baseline := testSample(t, 1, 0, 1, "success", "eligible", 20)
	for name, change := range map[string]func(sample) sample{
		"chain":         func(s sample) sample { s.Chain.ChainID = 8453; return s },
		"artifact":      func(s sample) sample { s.Artifact = domain.Hash{8}; return s },
		"receipt":       func(s sample) sample { s.Basis = "realized_receipt"; return s },
		"contradiction": func(s sample) sample { s.Economics.Net = big.NewInt(-1); return s },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Read(t.Context(), bytes.NewReader(journal(t, change(testSample(t, 1, 0, 1, "success", "eligible", 20)))), 10); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	changed := baseline
	changed.GasUsed++
	if _, err := Read(t.Context(), bytes.NewReader(journal(t, baseline, changed)), 10); err == nil {
		t.Fatal("conflicting ID accepted")
	}
	changed.ID = domain.Hash{2}
	if _, err := Read(t.Context(), bytes.NewReader(journal(t, baseline, changed)), 1); err == nil {
		t.Fatal("limit silently truncated report")
	}
	if _, err := Read(t.Context(), bytes.NewReader(append(journal(t, baseline), []byte("broken\n")...)), 10); err == nil {
		t.Fatal("complete corrupt record ignored")
	}
}

func TestReportDoesNotCountMissingOrChangedTransactionsAsSurvival(t *testing.T) {
	baseline := testSample(t, 1, 0, 1, "success", "eligible", 20)
	later := testSample(t, 2, 200, 1, "success", "eligible", 20)
	for name, change := range map[string]func(*sample){
		"call":   func(s *sample) { s.Call = json.RawMessage(`{"Data":"different"}`) },
		"source": func(s *sample) { s.Version = json.RawMessage(`{"Epoch":2}`) },
		"time":   func(s *sample) { s.DiscoveredAtNS++ },
	} {
		t.Run(name, func(t *testing.T) {
			variant := later
			change(&variant)
			result, err := Read(t.Context(), bytes.NewReader(journal(t, baseline, variant)), 10)
			if err != nil || result.Buckets[1].Paired.MismatchedCall != 1 || result.Buckets[1].Paired.PositiveLater != 0 {
				t.Fatal(result, err)
			}
		})
	}
	later.Status = "censored"
	later.StateBasis = "unverified"
	result, err := Read(t.Context(), bytes.NewReader(journal(t, baseline, later)), 10)
	if err != nil || result.Buckets[1].Paired.Censored != 1 || result.Buckets[1].KnownNet != 0 {
		t.Fatal(result, err)
	}
	result, err = Read(t.Context(), bytes.NewReader(journal(t, later)), 10)
	if err != nil || result.Buckets[0].Paired.MissingBaseline != 1 {
		t.Fatal(result, err)
	}
	// Field order changes must preserve matching, including integers above 2^53.
	a, _ := canonicalHash(json.RawMessage(`{"Nonce":9007199254740993,"Data":"x"}`))
	b, _ := canonicalHash(json.RawMessage(`{"Data":"x","Nonce":9007199254740993}`))
	c, _ := canonicalHash(json.RawMessage(`{"Data":"x","Nonce":9007199254740992}`))
	if a != b || a == c {
		t.Fatal("call precision/order lost")
	}
	if _, err := Read(t.Context(), strings.NewReader(""), 10); err == nil {
		t.Fatal("empty journal accepted")
	}
}

func TestReportMissingArrivalAfterInterruptedGroup(t *testing.T) {
	baseline := testSample(t, 1, 0, 1, "success", "eligible", 20)
	baseline.PlannedDelaysMS = []int64{200, 600}
	report, err := Read(t.Context(), bytes.NewReader(journal(t, baseline)), 10)
	if err != nil || len(report.Buckets) != 3 || report.Buckets[1].Samples != 0 || report.Buckets[1].Paired.MissingArrival != 1 || report.Buckets[2].Paired.MissingArrival != 1 {
		t.Fatal(report, err)
	}
}

func TestReportRetainsStrataSkipsQuoteErrorsAndBidSensitivity(t *testing.T) {
	s := testSample(t, 1, 0, 1, "success", "eligible", 20)
	s.Selection = &Selection{Mode: "stratified", Stratum: Stratum{Asset: domain.Address{1}, Hops: 3, Protocols: "2:3", GrossDigits: 2, Supported: true}}
	if err := json.Unmarshal([]byte(`{"status":"positive_budget","valuation_basis":"native_1_to_1","scenarios":[{"priority_fee_wei":0,"net_native_wei":20,"fits_fee_cap":true,"positive_net":true},{"priority_fee_wei":10,"net_native_wei":-10,"fits_fee_cap":true,"positive_net":false},{"priority_fee_wei":100,"net_native_wei":-1000,"fits_fee_cap":false,"positive_net":false}]}`), &s.Bidding); err != nil {
		t.Fatal(err)
	}
	raw := journal(t, s)
	summary := sample{Type: "sampling_summary", Basis: "executor_simulation", SamplingOverflow: 2, SamplingBuckets: []SamplingBucket{{Stratum: s.Selection.Stratum, Reasons: map[string]uint64{"interval": 4}, MaxGross: big.NewInt(900)}}}
	var tail bytes.Buffer
	if err := json.NewEncoder(&tail).Encode(summary); err != nil {
		t.Fatal(err)
	}
	report, err := Read(t.Context(), bytes.NewReader(append(raw, tail.Bytes()...)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sampling) != 1 || report.Sampling[0].Skipped["interval"] != 4 || report.Sampling[0].Selected != 1 || report.Sampling[0].GrossError.Int64() != -3 || report.SamplingOverflow != 2 {
		t.Fatal(report)
	}
	if len(report.Bids) != 3 || report.Bids[0].Positive != 1 || report.Bids[1].NonPositive != 1 || report.Bids[2].CapExceeded != 1 {
		t.Fatal(report.Bids)
	}
}
