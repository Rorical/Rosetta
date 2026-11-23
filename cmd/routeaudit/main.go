// Command routeaudit diagnoses historical route coverage from local pool captures.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Rorical/Rosetta/internal/pipeline"
)

type options struct {
	capture, cases, coverage, search, costs, output string
	automatic                                       bool
	paper, candidates, inventory, executionPaper    string
	maxCases                                        int
}

func main() {
	if err := runCLI(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "routeaudit:", err)
		os.Exit(1)
	}
}
func runCLI(ctx context.Context, args []string, stdout io.Writer) error {
	cfg := options{}
	flags := flag.NewFlagSet("routeaudit", flag.ContinueOnError)
	flags.StringVar(&cfg.capture, "capture", "", "local -replay-out JSONL capture")
	flags.StringVar(&cfg.cases, "cases", "", "known historical route positions JSON")
	flags.StringVar(&cfg.coverage, "coverage", "", "optional active pool/token inventory JSON for omission analysis")
	flags.StringVar(&cfg.search, "search-config", "", "same funding bounds and path limits as the runtime")
	flags.StringVar(&cfg.costs, "research-config", "", "explicit cost assumptions; omission leaves net profit unknown")
	flags.StringVar(&cfg.output, "out", "", "new JSONL output file; empty writes stdout")
	flags.BoolVar(&cfg.automatic, "auto", false, "extract supported closed swap cycles from canonical captured receipts")
	flags.StringVar(&cfg.paper, "paper", "", "local observations JSONL, optionally gzipped")
	flags.StringVar(&cfg.candidates, "candidates", "", "complete local raw candidate JSONL, optionally gzipped")
	flags.StringVar(&cfg.executionPaper, "execution-paper", "", "optional local executor paper journal, optionally gzipped (auto only)")
	flags.StringVar(&cfg.inventory, "inventory", "", "optional creation index for immutable pool currency metadata")
	flags.IntVar(&cfg.maxCases, "max-cases", 500, "maximum automatically extracted cycles; reports truncation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if cfg.capture == "" || (!cfg.automatic && cfg.cases == "") || cfg.search == "" {
		return fmt.Errorf("capture, search-config and either cases or auto are required")
	}
	if !cfg.automatic && cfg.executionPaper != "" {
		return fmt.Errorf("execution-paper requires auto")
	}
	policy, err := pipeline.LoadSearchPolicy(cfg.search)
	if err != nil {
		return err
	}
	costs, err := pipeline.LoadResearchPolicy(cfg.costs)
	if err != nil {
		return err
	}
	if cfg.automatic {
		if cfg.cases != "" || cfg.maxCases < 1 || cfg.coverage != "" {
			return fmt.Errorf("auto requires positive max-cases and uses captured coverage; omit cases and coverage")
		}
		return runAutomatic(ctx, cfg, stdout, policy, costs)
	}
	cases, err := loadCases(cfg.cases)
	if err != nil {
		return err
	}
	coverage, err := loadCoverage(cfg.coverage, cases.ChainID)
	if err != nil {
		return err
	}
	output := stdout
	if cfg.output != "" {
		file, err := os.OpenFile(cfg.output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		output = file
	}
	capture, err := os.Open(cfg.capture)
	if err != nil {
		return err
	}
	defer capture.Close()
	return replayAudit(ctx, capture, json.NewEncoder(output), cases, coverage, policy, costs)
}
