package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIRejectsBadInputWithoutPartialReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	if err := os.WriteFile(path, []byte("{\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"-input", path}, {"-input", path, "-since", "yesterday"}} {
		var output bytes.Buffer
		if err := runCLI(t.Context(), args, &output); err == nil || output.Len() != 0 {
			t.Fatalf("args %v: error=%v output=%s", args, err, output.String())
		}
	}
}
