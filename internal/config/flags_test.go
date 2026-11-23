package config

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestParseRejectsInvalidCLIInput(t *testing.T) {
	for _, args := range [][]string{
		{"-unknown"}, {"-blocks", "not-a-number"}, {"-rpc-wait", "invalid"}, {"run"},
	} {
		if _, err := Parse(args, io.Discard); err == nil {
			t.Fatalf("accepted invalid command input %q", args)
		}
	}
}

func TestParseDoesNotReusePreviousInvocation(t *testing.T) {
	// Multiple embedded invocations must not share flag values or register
	// flags on the process-wide flag set.
	first, err := Parse([]string{"-pool-config", "pools.json", "-fast-first-index", "0", "-rpc-max-calls", "25"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if first.PoolConfig != "pools.json" || first.FastFirstIndex != 0 || first.RPCMaxCalls != 25 ||
		second.PoolConfig != "" || second.FastFirstIndex != -1 || second.RPCMaxCalls != 0 {
		t.Fatal("CLI invocations lost explicit options or shared mutable defaults")
	}
}

func TestHelpDescribesRPCEntry(t *testing.T) {
	var output bytes.Buffer
	_, err := Parse([]string{"-h"}, &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
	for _, option := range []string{"-rpc", "-fast-feed", "-pool-catalog", "-plan-out"} {
		if !strings.Contains(output.String(), option) {
			t.Errorf("official command help omits %s", option)
		}
	}
}

func TestRunRootOwnsItsOutputs(t *testing.T) {
	base := []string{"-run-root", "/state", "-control-dir", "/state/control", "-search-config", "search.json"}
	cfg, err := Parse(base, io.Discard)
	if err != nil || cfg.RunRoot != "/state" {
		t.Fatalf("valid run root: %q %v", cfg.RunRoot, err)
	}
	for _, extra := range [][]string{
		{"-candidate-out", "c.jsonl"}, {"-research-out", "o.jsonl"}, {"-replay-out", "r.jsonl"},
		{"-pool-snapshot-out", "p.json"}, {"-metrics-out", "m.json"},
	} {
		if _, err := Parse(append(slices.Clone(base), extra...), io.Discard); err == nil || !strings.Contains(err.Error(), extra[0]) {
			t.Errorf("run root accepted explicit %s: %v", extra[0], err)
		}
	}
	for _, missing := range []string{"-control-dir", "-search-config"} {
		var args []string
		for i := 0; i < len(base); i += 2 {
			if base[i] != missing {
				args = append(args, base[i], base[i+1])
			}
		}
		if _, err := Parse(args, io.Discard); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("run root accepted without %s: %v", missing, err)
		}
	}
	// Deployments without a run root keep naming each output.
	if _, err := Parse([]string{"-candidate-out", "c.jsonl", "-replay-out", "r.jsonl", "-metrics-out", "m.json"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}
