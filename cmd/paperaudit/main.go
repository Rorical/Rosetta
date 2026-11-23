// Command paperaudit compares local execution paper scenarios without RPC.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Rorical/Rosetta/internal/paperreport"
)

func main() {
	if err := runCLI(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "paperaudit:", err)
		os.Exit(1)
	}
}
func runCLI(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paperaudit", flag.ContinueOnError)
	live := flags.Bool("live", false, "summarize canonical paid settlements from a reactive journal")
	input := flags.String("input", "", "local execution paper JSONL journal")
	limit := flags.Int("max-samples", 100000, "maximum unique samples (1..200000); exceeding it fails")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *input == "" || flags.NArg() != 0 {
		return fmt.Errorf("-input journal.jsonl is required; positional arguments are not supported")
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer file.Close()
	var report any
	if *live {
		report, err = paperreport.ReadLive(ctx, file, *limit)
	} else {
		report, err = paperreport.Read(ctx, file, *limit)
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}
