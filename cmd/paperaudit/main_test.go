package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLIRequiresJournalAndEmitsNoPartialReportOnError(t *testing.T) {
	var output bytes.Buffer
	if err := runCLI(t.Context(), nil, &output); err == nil {
		t.Fatal("missing input accepted")
	}
	path := filepath.Join(t.TempDir(), "paper.jsonl")
	if err := os.WriteFile(path, []byte("invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(t.Context(), []string{"-input", path}, &output); err == nil || !strings.Contains(err.Error(), "paper line 1") || output.Len() != 0 {
		t.Fatal("partial or misleading report", err, output.String())
	}
}
