package main

import (
	"context"
	"strings"
	"testing"
)

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.depth != 10 {
		t.Errorf("depth = %d, want 10", cfg.depth)
	}
	if cfg.cycles != 20 {
		t.Errorf("cycles = %d, want 20", cfg.cycles)
	}
	if cfg.repeat != 20 {
		t.Errorf("repeat = %d, want 20", cfg.repeat)
	}
	if cfg.warmup != 1 {
		t.Errorf("warmup = %d, want 1", cfg.warmup)
	}
	if cfg.k != 200 {
		t.Errorf("k = %d, want 200", cfg.k)
	}
	if got := cfg.notional.String(); got != "1000000000000000000" {
		t.Errorf("notional = %s, want 1000000000000000000", got)
	}
	if cfg.maxCycles != 80 {
		t.Errorf("max cycles = %d, want 80", cfg.maxCycles)
	}
	if cfg.poolTopK != 80 {
		t.Errorf("pool top k = %d, want 80", cfg.poolTopK)
	}
	if !cfg.timing {
		t.Error("timing = false, want true")
	}
}

func TestParseFlagsDerivedTableBudget(t *testing.T) {
	cfg, err := parseFlags([]string{"-cycles", "5"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.maxCycles != 20 || cfg.poolTopK != 20 {
		t.Fatalf("derived budgets = %d/%d, want 20/20", cfg.maxCycles, cfg.poolTopK)
	}

	cfg, err = parseFlags([]string{"-cycles", "5", "-max-cycles", "7", "-pool-top-k", "9"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.maxCycles != 7 || cfg.poolTopK != 9 {
		t.Fatalf("explicit budgets = %d/%d, want 7/9", cfg.maxCycles, cfg.poolTopK)
	}
}

func TestParseFlagsRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "depth below range", args: []string{"-depth", "8"}, want: "-depth 8"},
		{name: "depth above range", args: []string{"-depth", "17"}, want: "-depth 17"},
		{name: "no cycles", args: []string{"-cycles", "0"}, want: "-cycles 0"},
		{name: "no repeats", args: []string{"-repeat", "0"}, want: "-repeat 0"},
		{name: "negative warmup", args: []string{"-warmup", "-1"}, want: "-warmup -1"},
		{name: "zero k", args: []string{"-k", "0"}, want: "-k 0"},
		{name: "zero max cycles", args: []string{"-max-cycles", "-1"}, want: "-max-cycles -1"},
		{name: "zero pool top k", args: []string{"-pool-top-k", "-1"}, want: "-pool-top-k -1"},
		{name: "zero notional", args: []string{"-notional", "0"}, want: "-notional"},
		{name: "bad notional", args: []string{"-notional", "1e18"}, want: "-notional"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseFlags(test.args)
			if err == nil {
				t.Fatalf("parseFlags(%q) succeeded, want error", test.args)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
}

func TestRunReportsKnownRing(t *testing.T) {
	cfg, err := parseFlags([]string{
		"-depth", "9",
		"-cycles", "2",
		"-repeat", "3",
		"-warmup", "1",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}

	var out strings.Builder
	if err := run(context.Background(), cfg, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Two rings give four directed cycles, so one measured evaluation quotes
	// 4 cycles * 9 legs; three measured evaluations give 108 quote samples.
	wants := []string{
		"synthetic timing-only; not on-chain accuracy",
		"table depth=9 cycles=2 table_cycles=4 opportunities=4",
		"search n=3 p50=",
		"quote n=108 p50=",
		"known found=true rank=1 net=",
		"top1=true",
		"synthetic timing-only depth=9 cycles=2 opportunities=4 found=true rank=1 top1=true",
	}
	text := out.String()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
}

func TestRunTimingOffSkipsTimingLines(t *testing.T) {
	cfg, err := parseFlags([]string{
		"-depth", "9",
		"-cycles", "1",
		"-repeat", "5",
		"-warmup", "2",
		"-timing=false",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if warmup, repeat := cfg.measurements(); warmup != 0 || repeat != 1 {
		t.Fatalf("measurements = %d/%d, want 0/1", warmup, repeat)
	}

	var out strings.Builder
	if err := run(context.Background(), cfg, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(out.String(), "search n=") || strings.Contains(out.String(), "quote n=") {
		t.Fatalf("timing lines printed with -timing=false:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "found=true rank=1") {
		t.Fatalf("known ring missing with -timing=false:\n%s", out.String())
	}
}
