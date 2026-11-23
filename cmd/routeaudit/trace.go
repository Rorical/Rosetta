package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

type traceData struct {
	frames        map[uint64][]backtest.PrefixEvidence
	gaps          []backtest.TraceGap
	candidates    []quoteEvidence
	searches      map[domain.StateVersion][]searchEvidence
	candidateFile bool
}
type searchEvidence struct {
	CompletedAtNS int64                 `json:"completed_at_ns"`
	Observation   arb.SearchObservation `json:"observation"`
}
type quoteEvidence struct {
	Opportunity arb.Opportunity      `json:"opportunity"`
	Timing      pipeline.QuoteTiming `json:"timing"`
}

func scanJSONL(path string, visit func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var reader io.Reader = f
	if len(path) > 3 && path[len(path)-3:] == ".gz" {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader = gz
	}
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 65536), 128<<20)
	line := 0
	for s.Scan() {
		line++
		if err = visit(s.Bytes()); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
	}
	return s.Err()
}
func loadTrace(paper, candidates string, chainID uint64) (traceData, error) {
	trace := traceData{frames: map[uint64][]backtest.PrefixEvidence{}, searches: map[domain.StateVersion][]searchEvidence{}, candidateFile: candidates != ""}
	if paper != "" {
		if err := scanJSONL(paper, func(raw []byte) error {
			var row struct {
				Type    string              `json:"type"`
				ChainID uint64              `json:"chain_id"`
				Version domain.StateVersion `json:"version"`
				Details json.RawMessage     `json:"details"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				return err
			}
			if row.ChainID != chainID {
				return fmt.Errorf("paper chain mismatch")
			}
			switch row.Type {
			case "prefix":
				var p pipeline.PrefixObservation
				if err := json.Unmarshal(row.Details, &p); err != nil {
					return err
				}
				trace.frames[p.Version.BlockNumber] = append(trace.frames[p.Version.BlockNumber], backtest.PrefixEvidence{Version: p.Version, First: p.FirstTxIndex, Count: p.TransactionCount, Hashes: p.Transactions, Received: p.ReceivedAtNS, Applied: p.AppliedAtNS})
			case "prefix_gap":
				var gap struct {
					First int64 `json:"first_received_at_ns"`
					Last  int64 `json:"last_received_at_ns"`
				}
				if err := json.Unmarshal(row.Details, &gap); err != nil {
					return err
				}
				trace.gaps = append(trace.gaps, backtest.TraceGap{First: gap.First, Last: gap.Last})
			case "search":
				var details searchEvidence
				if err := json.Unmarshal(row.Details, &details); err != nil {
					return err
				}
				trace.searches[row.Version] = append(trace.searches[row.Version], details)
			}
			return nil
		}); err != nil {
			return trace, err
		}
	}
	if candidates != "" {
		if err := scanJSONL(candidates, func(raw []byte) error {
			var q quoteEvidence
			if err := json.Unmarshal(raw, &q); err != nil {
				return err
			}
			trace.candidates = append(trace.candidates, q)
			return nil
		}); err != nil {
			return trace, err
		}
	}
	return trace, nil
}
