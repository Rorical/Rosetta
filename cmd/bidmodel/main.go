// Command bidmodel validates portable tree artifacts and checks local inference.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Rorical/Rosetta/internal/bidmodel"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "bidmodel:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	flags := flag.NewFlagSet("bidmodel", flag.ContinueOnError)
	path := flags.String("model", "", "portable artifact")
	chain := flags.Uint64("chain", 0, "required chain ID")
	code := flags.String("code-hash", "", "required executor code hash")
	precheck := flags.Bool("precheck", false, "executor price precheck enabled")
	predict := flags.Bool("predict", false, "read feature arrays as JSONL from stdin")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || *chain == 0 || *code == "" || flags.NArg() != 0 {
		return fmt.Errorf("model, chain and code-hash are required")
	}
	var store bidmodel.Store
	if err := store.Load(*path, bidmodel.Scope{ChainID: *chain, CodeHash: *code, Precheck: *precheck}, time.Now()); err != nil {
		return err
	}
	model, reason := store.Snapshot(time.Now(), false)
	if model == nil {
		return fmt.Errorf("model unavailable: %s", reason)
	}
	encoder := json.NewEncoder(output)
	if !*predict {
		return encoder.Encode(map[string]any{"sha256": model.ID, "approved": model.Approved, "schema": model.Schema})
	}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var values []float64
		if err := json.Unmarshal(scanner.Bytes(), &values); err != nil {
			return err
		}
		if len(values) != bidmodel.FeatureCount {
			return fmt.Errorf("incorrect feature count")
		}
		var f bidmodel.Features
		copy(f[:], values)
		p, supported := model.Predict(f)
		if err := encoder.Encode(map[string]any{"supported": supported, "win": p.Win, "revert_ratio": p.RevertRatio, "delay_ms": p.DelayMS}); err != nil {
			return err
		}
	}
	return scanner.Err()
}
