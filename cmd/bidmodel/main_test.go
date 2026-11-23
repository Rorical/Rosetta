package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLIRequiresExecutionScope(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), []string{"-model", "unknown"}, strings.NewReader(""), &out); err == nil || out.Len() != 0 {
		t.Fatal("unscoped model accepted")
	}
}
