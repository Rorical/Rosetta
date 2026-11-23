// Command bidaudit scores recorded production predictions, without RPC or signing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Rorical/Rosetta/internal/bidreport"
)

func main() {
	if err := runCLI(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bidaudit:", err)
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("bidaudit", flag.ContinueOnError)
	input := flags.String("input", "", "reactive JSONL journal")
	competition := flags.String("competition", "", "optional competition JSONL for non-causal failure evidence")
	limit := flags.Int("max-records", 100000, "maximum retained hashes across predictions, outcomes and voids (1..200000)")
	since := flags.String("since", "", "inclusive RFC3339 submission time")
	until := flags.String("until", "", "exclusive RFC3339 submission time")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || flags.NArg() != 0 {
		return fmt.Errorf("-input is required; positional arguments are not supported")
	}
	opts := bidreport.Options{Limit: *limit}
	for _, field := range []struct {
		text string
		to   *time.Time
	}{{*since, &opts.Since}, {*until, &opts.Until}} {
		if field.text != "" {
			v, err := time.Parse(time.RFC3339, field.text)
			if err != nil {
				return fmt.Errorf("submission time: %w", err)
			}
			*field.to = v
		}
	}
	if *competition != "" {
		f, err := os.Open(*competition)
		if err != nil {
			return err
		}
		opts.Competition, err = bidreport.ReadCompetition(ctx, f, *limit)
		f.Close()
		if err != nil {
			return err
		}
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer file.Close()
	report, err := bidreport.Read(ctx, file, opts)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}
