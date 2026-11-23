package paperreport

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"

	"github.com/Rorical/Rosetta/internal/domain"
)

type bucketKey struct {
	delay int64
	asset string
}
type accumulator struct {
	samplingWait                                       []uint64
	rpcCalls, snapshot, setup, feePrefetch, simulation []uint64
	bucket                                             Bucket
	gas, delay, queue, duration                        []uint64
}
type comparison struct {
	plannedDelays   []int64
	key             bucketKey
	status          string
	known, verified bool
	positive        bool
	call, version   [32]byte
	hasCall         bool
	discovered      int64
}

func newAccumulator(key bucketKey) *accumulator {
	return &accumulator{bucket: Bucket{DelayMS: key.delay, Asset: key.asset, Status: make(map[string]int), NetStatus: make(map[string]int), NetSum: new(big.Int), GrossErrorSum: new(big.Int)}}
}
func canonicalHash(raw json.RawMessage) ([32]byte, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return [32]byte{}, false
	}
	// Use RawMessage for values, retaining uint256 precision and field ordering
	// independence at the object boundary.
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return [32]byte{}, false
	}
	normalized, _ := json.Marshal(object)
	return sha256.Sum256(normalized), true
}
func (a *accumulator) add(s sample) (comparison, error) {
	switch s.Status {
	case "success", "reverted", "unsupported", "rejected_funding", "rejected_fees", "rejected_fee_unknown", "rejected_token", "unavailable", "censored":
	default:
		return comparison{}, fmt.Errorf("unknown execution status %q", s.Status)
	}
	b := &a.bucket
	b.Samples++
	b.Status[s.Status]++
	c := comparison{plannedDelays: s.PlannedDelaysMS, key: bucketKey{b.DelayMS, b.Asset}, status: s.Status, discovered: s.DiscoveredAtNS}
	c.call, c.hasCall = canonicalHash(s.Call)
	c.version, _ = canonicalHash(s.Version)
	c.verified = (s.StateBasis == "discovery_snapshot" && s.DelayMS == 0 || s.StateBasis == "later_observed_prefix" && s.DelayMS > 0) && (s.Status == "success" || s.Status == "reverted")
	netStatus := "unknown"
	if c.verified {
		b.Verified++
		if s.Economics != nil && s.Economics.Basis == "executor_simulation" && s.Economics.Status != "unknown" {
			switch s.Economics.Status {
			case "eligible", "below_minimum", "non_positive":
			default:
				return c, fmt.Errorf("unknown net status %q", s.Economics.Status)
			}
			if s.Economics.Net == nil || (s.Economics.Net.Sign() > 0) != (s.Economics.Status != "non_positive") {
				return c, fmt.Errorf("net value contradicts status")
			}
			c.known, c.positive = true, s.Economics.Net.Sign() > 0
			netStatus = s.Economics.Status
			b.KnownNet++
			b.NetSum.Add(b.NetSum, s.Economics.Net)
		}
		if s.Status == "success" && s.QuotedGross != nil && s.Profit != nil && s.LoanFee != nil {
			difference := new(big.Int).Add(s.Profit, s.LoanFee)
			difference.Sub(difference, s.QuotedGross)
			b.GrossErrorSum.Add(b.GrossErrorSum, difference)
			b.QuoteComparisons++
		}
	}
	b.NetStatus[netStatus]++
	if s.Selection != nil && s.Selection.WaitNS > 0 {
		a.samplingWait = append(a.samplingWait, uint64(s.Selection.WaitNS))
	}
	if s.GasUsed > 0 {
		a.gas = append(a.gas, s.GasUsed)
	}
	if s.DelayMS > 0 && s.ObservedAtNS > 0 && s.ActualDelayNS > 0 {
		a.delay = append(a.delay, uint64(s.ActualDelayNS))
	}
	if s.QueuedAtNS > 0 && s.StartedAtNS >= s.QueuedAtNS {
		a.queue = append(a.queue, uint64(s.StartedAtNS-s.QueuedAtNS))
	}
	if s.FinishedAtNS >= s.StartedAtNS && s.StartedAtNS > 0 {
		a.duration = append(a.duration, uint64(s.FinishedAtNS-s.StartedAtNS))
	}
	// Early journals lost the dedicated client's observer to the shared budget.
	// A verified RPC execution cannot have zero requests; retain the sample but
	// leave its missing request measurement out of the distribution.
	if s.RPCCalls != nil && (*s.RPCCalls > 0 || !c.verified) {
		a.rpcCalls = append(a.rpcCalls, *s.RPCCalls)
	}
	if s.SnapshotNS > 0 {
		a.snapshot = append(a.snapshot, uint64(s.SnapshotNS))
	}
	if s.SetupNS > 0 {
		a.setup = append(a.setup, uint64(s.SetupNS))
	}
	if s.FeePrefetchNS > 0 {
		a.feePrefetch = append(a.feePrefetch, uint64(s.FeePrefetchNS))
	}
	if s.ExecutionNS > 0 {
		a.simulation = append(a.simulation, uint64(s.ExecutionNS))
	}
	return c, nil
}

func summarizePairs(groups map[domain.Hash]map[int64]comparison, buckets map[bucketKey]*accumulator) {
	for _, group := range groups {
		baseline, ok := group[0]
		if ok {
			for _, delay := range baseline.plannedDelays {
				if _, exists := group[delay]; exists {
					continue
				}
				key := bucketKey{delay: delay, asset: baseline.key.asset}
				if buckets[key] == nil {
					buckets[key] = newAccumulator(key)
				}
				buckets[key].bucket.Paired.MissingArrival++
			}
		}
		for delay, later := range group {
			if delay == 0 {
				continue
			}
			p := &buckets[later.key].bucket.Paired
			if !ok {
				p.MissingBaseline++
				continue
			}
			if !baseline.verified || !baseline.known || !baseline.positive {
				p.BaselineNotPositive++
				continue
			}
			p.PositiveBaseline++
			switch {
			case later.status == "censored":
				p.Censored++
			case !later.verified:
				p.Unknown++
			case !baseline.hasCall || !later.hasCall || baseline.call != later.call || baseline.version != later.version || baseline.discovered != later.discovered || baseline.key.asset != later.key.asset:
				p.MismatchedCall++
			case later.status == "reverted":
				p.Reverted++
			case !later.known:
				p.Unknown++
			case later.positive:
				p.PositiveLater++
			default:
				p.NonPositiveLater++
			}
		}
	}
}
func distribution(values []uint64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return Distribution{Count: len(values), P50: values[(len(values)*50+99)/100-1], P95: values[(len(values)*95+99)/100-1], Max: values[len(values)-1]}
}
func finishBuckets(buckets map[bucketKey]*accumulator) []Bucket {
	result := make([]Bucket, 0, len(buckets))
	for _, a := range buckets {
		a.bucket.SamplingWait = distribution(a.samplingWait)
		a.bucket.RPCCalls = distribution(a.rpcCalls)
		a.bucket.Snapshot = distribution(a.snapshot)
		a.bucket.Setup = distribution(a.setup)
		a.bucket.FeePrefetch = distribution(a.feePrefetch)
		a.bucket.Simulation = distribution(a.simulation)
		a.bucket.Gas = distribution(a.gas)
		a.bucket.ActualDelay = distribution(a.delay)
		a.bucket.Queue = distribution(a.queue)
		a.bucket.Execution = distribution(a.duration)
		result = append(result, a.bucket)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].DelayMS != result[j].DelayMS {
			return result[i].DelayMS < result[j].DelayMS
		}
		return result[i].Asset < result[j].Asset
	})
	return result
}
