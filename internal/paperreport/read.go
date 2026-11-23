package paperreport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Read accepts a single chain/artifact journal. It rejects complete corrupt
// lines; an unfinished final append is reported and excluded. The explicit
// sample bound fails rather than silently reporting a biased partial window.
func Read(ctx context.Context, input io.Reader, maxSamples int) (Report, error) {
	var report Report
	var diagnostic diagnostics
	if maxSamples < 1 || maxSamples > 200000 {
		return report, fmt.Errorf("max-samples must be within 1..200000")
	}
	report.Basis = "executor_simulation"
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			report.TornTail = true
			return len(data), nil, nil
		}
		return 0, nil, nil
	})
	seen := make(map[domain.Hash][32]byte)
	buckets := make(map[bucketKey]*accumulator)
	groups := make(map[domain.Hash]map[int64]comparison)
	line := 0
	for scanner.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return report, err
		}
		var record sample
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return report, fmt.Errorf("paper line %d: %w", line, err)
		}
		if record.Basis != report.Basis {
			return report, fmt.Errorf("paper line %d: unsupported evidence basis", line)
		}
		if line == 1 {
			if record.Type != "session" || record.Schema != 1 || record.Chain.ChainID == 0 {
				return report, fmt.Errorf("paper journal requires schema 1 session header")
			}
			report.Chain, report.Artifact = record.Chain, record.Artifact
			continue
		}
		if record.Type == "sampling_summary" {
			diagnostic.skipped(record.SamplingBuckets)
			report.SamplingOverflow += record.SamplingOverflow
			continue
		}
		if record.Type == "sampling_gap" {
			report.Skipped += record.Skipped
			continue
		}
		// Shadow intent replays are selected by the reactive path, not by the
		// paper sampler; mixing them in would bias the sampled distributions.
		if record.Type == "reactive_verification" {
			continue
		}
		if record.Type != "execution" || record.Schema != 1 || record.Chain != report.Chain || record.Artifact != report.Artifact || record.ID == (domain.Hash{}) || record.DelayMS < 0 || record.DelayMS > 2000 {
			return report, fmt.Errorf("paper line %d: invalid sample identity", line)
		}
		previous := int64(0)
		if len(record.PlannedDelaysMS) > 4 {
			return report, fmt.Errorf("paper line %d: too many planned delays", line)
		}
		for _, delay := range record.PlannedDelaysMS {
			if delay <= previous || delay > 2000 {
				return report, fmt.Errorf("paper line %d: invalid planned delays", line)
			}
			previous = delay
		}
		fingerprint := sha256.Sum256(scanner.Bytes())
		if previous, ok := seen[record.ID]; ok {
			if previous != fingerprint {
				return report, fmt.Errorf("paper line %d: conflicting sample ID", line)
			}
			report.Duplicates++
			continue
		}
		if report.Samples == maxSamples {
			return report, fmt.Errorf("paper exceeds max-samples=%d; select a smaller input window", maxSamples)
		}
		seen[record.ID] = fingerprint
		report.Samples++
		diagnostic.add(record)
		key := bucketKey{delay: record.DelayMS, asset: "unknown"}
		if record.Candidate != nil {
			key.asset = record.Candidate.Cycle.Start.Hex()
		}
		bucket := buckets[key]
		if bucket == nil {
			bucket = newAccumulator(key)
			buckets[key] = bucket
		}
		compared, err := bucket.add(record)
		if err != nil {
			return report, fmt.Errorf("paper line %d: %w", line, err)
		}
		if record.Group == (domain.Hash{}) {
			if record.DelayMS != 0 {
				return report, fmt.Errorf("paper line %d: delayed sample lacks group", line)
			}
			record.Group = record.ID // Original journals contain discovery-only records.
		}
		group := groups[record.Group]
		if group == nil {
			group = make(map[int64]comparison)
			groups[record.Group] = group
		}
		if _, ok := group[record.DelayMS]; ok {
			return report, fmt.Errorf("paper line %d: duplicate group scenario", line)
		}
		group[record.DelayMS] = compared
	}
	if err := scanner.Err(); err != nil {
		return report, fmt.Errorf("read paper journal: %w", err)
	}
	if line == 0 {
		return report, fmt.Errorf("empty or incomplete paper journal header")
	}
	summarizePairs(groups, buckets)
	report.Buckets = finishBuckets(buckets)
	diagnostic.finish(&report)
	return report, nil
}
