package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

// Only the immutable journal evidence is read. The audit never starts a new
// simulation or turns a profitable later scenario into a discovery-time fill.
type executionEvidence struct {
	Schema     int                         `json:"schema"`
	Basis      string                      `json:"basis"`
	Type       string                      `json:"type"`
	Chain      domain.ChainIdentity        `json:"chain"`
	ID         domain.Hash                 `json:"sample_id"`
	Group      domain.Hash                 `json:"group_id"`
	Version    domain.StateVersion         `json:"version"`
	Candidate  *arb.Opportunity            `json:"candidate,omitempty"`
	DelayMS    int64                       `json:"delay_ms"`
	Discovered int64                       `json:"discovered_at_ns"`
	Finished   int64                       `json:"finished_at_ns"`
	Status     string                      `json:"status"`
	StateBasis string                      `json:"state_basis"`
	Economics  *pipeline.ResearchEconomics `json:"economics,omitempty"`
}
type skipEvidence struct {
	Cycle   graph.Cycle         `json:"max_gross_cycle"`
	Version domain.StateVersion `json:"max_gross_version"`
	AtNS    int64               `json:"max_gross_observed_at_ns"`
	Reason  string              `json:"max_gross_reason,omitempty"`
	Input   *big.Int            `json:"max_gross_input,omitempty"`
	Reasons map[string]uint64   `json:"reasons,omitempty"`
	Gross   *big.Int            `json:"max_quoted_gross_raw"`
}
type executionTrace struct {
	Provided bool
	Samples  []executionEvidence
	Skips    []skipEvidence
}

func loadExecutionTrace(path string, chain uint64) (executionTrace, error) {
	trace := executionTrace{Provided: path != ""}
	if path == "" {
		return trace, nil
	}
	session := false
	var identity domain.ChainIdentity
	var artifact domain.Hash
	seen := map[domain.Hash][32]byte{}
	err := scanJSONL(path, func(raw []byte) error {
		var row struct {
			Type     string               `json:"type"`
			Schema   int                  `json:"schema"`
			Basis    string               `json:"basis"`
			Chain    domain.ChainIdentity `json:"chain"`
			Buckets  []skipEvidence       `json:"buckets"`
			Artifact domain.Hash          `json:"creation_code_hash"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		if row.Basis != "executor_simulation" {
			return fmt.Errorf("unsupported execution journal basis")
		}
		if !session {
			if row.Type != "session" || row.Schema != 1 || row.Chain.ChainID != chain {
				return fmt.Errorf("execution journal requires matching schema 1 session")
			}
			identity, artifact = row.Chain, row.Artifact
			session = true
			return nil
		}
		switch row.Type {
		case "sampling_summary":
			for _, bucket := range row.Buckets {
				// A legacy maximum has a proven reason only when every quote
				// in its bucket was skipped for that same single reason.
				if bucket.Reason == "" && len(bucket.Reasons) == 1 {
					for reason, count := range bucket.Reasons {
						if count > 0 {
							bucket.Reason = reason
						}
					}
				}
				trace.Skips = append(trace.Skips, bucket)
			}
		case "sampling_gap":
			// Aggregate loss cannot identify a particular historical route.
		case "execution":
			if row.Schema != 1 || row.Chain != identity || row.Artifact != artifact {
				return fmt.Errorf("execution sample schema, chain or artifact mismatch")
			}
			var sample executionEvidence
			if err := json.Unmarshal(raw, &sample); err != nil {
				return err
			}
			if sample.ID == (domain.Hash{}) {
				return fmt.Errorf("execution sample has no identity")
			}
			fingerprint := sha256.Sum256(raw)
			if previous, ok := seen[sample.ID]; ok {
				if previous != fingerprint {
					return fmt.Errorf("conflicting execution sample identity")
				}
				return nil
			}
			seen[sample.ID] = fingerprint
			trace.Samples = append(trace.Samples, sample)
		default:
			return fmt.Errorf("unsupported execution journal record %q", row.Type)
		}
		return nil
	})
	if err == nil && !session {
		err = fmt.Errorf("empty execution journal")
	}
	return trace, err
}

type executionAuditResult struct {
	Status              string              `json:"status"`
	MatchBasis          string              `json:"match_basis"`
	BaselinePositive    int                 `json:"discovery_positive"`
	BaselineNonPositive int                 `json:"discovery_non_positive"`
	BaselineUnknown     int                 `json:"discovery_unknown"`
	ReadyBeforeDeadline int                 `json:"discovery_positive_finished_before_deadline"`
	Samples             []executionEvidence `json:"samples,omitempty"`
	Skips               []skipEvidence      `json:"skip_witnesses,omitempty"`
}

func (a *automaticAudit) validEvidencePrefix(c *cycleAudit, v backtest.Visibility, version domain.StateVersion, at int64) bool {
	if v.Status != "observable" || at < v.Available || at >= v.Deadline {
		return false
	}
	f, ok := a.visibleFrame(c, v, version)
	return ok && f.Applied <= at
}

func (a *automaticAudit) executionAudit(c *cycleAudit, v backtest.Visibility, trace executionTrace, tokens func(domain.PoolKey) (domain.Address, domain.Address, bool)) executionAuditResult {
	out := executionAuditResult{Status: "not_provided", MatchBasis: "directed_route_at_canonical_visible_prefix; amounts_may_differ"}
	if !trace.Provided {
		return out
	}
	out.Status = "not_observed"
	if v.Status != "observable" {
		out.Status = "not_observable"
		return out
	}
	for _, r := range trace.Samples {
		if r.Candidate == nil || !a.validEvidencePrefix(c, v, r.Version, r.Discovered) || !graph.SameDirectedCycle(r.Candidate.Cycle, c.cycle.Cycle, tokens) {
			continue
		}
		out.Samples = append(out.Samples, r)
		out.Status = "sampled"
		if r.DelayMS != 0 {
			continue
		}
		if r.Status == "success" && r.StateBasis == "discovery_snapshot" && r.Economics != nil && r.Economics.Net != nil && r.Economics.Status != pipeline.NetUnknown {
			if r.Economics.Net.Sign() > 0 {
				out.BaselinePositive++
				if r.Finished >= r.Discovered && r.Finished < v.Deadline {
					out.ReadyBeforeDeadline++
				}
			} else {
				out.BaselineNonPositive++
			}
		} else if r.Status == "reverted" && r.StateBasis == "discovery_snapshot" {
			out.BaselineNonPositive++
		} else {
			out.BaselineUnknown++
		}
	}
	for _, r := range trace.Skips {
		// Reasons on legacy buckets aggregate different quotes. Only the explicit
		// maximum witness proves the skip reason for this route/version/time.
		if r.Reason == "" || !a.validEvidencePrefix(c, v, r.Version, r.AtNS) || !graph.SameDirectedCycle(r.Cycle, c.cycle.Cycle, tokens) {
			continue
		}
		out.Skips = append(out.Skips, r)
		if out.Status != "sampled" {
			out.Status = "sampling_skipped"
		}
	}
	sort.Slice(out.Samples, func(i, j int) bool {
		x, y := out.Samples[i], out.Samples[j]
		if x.Discovered != y.Discovered {
			return x.Discovered < y.Discovered
		}
		if x.DelayMS != y.DelayMS {
			return x.DelayMS < y.DelayMS
		}
		return x.ID.Hex() < y.ID.Hex()
	})
	return out
}
