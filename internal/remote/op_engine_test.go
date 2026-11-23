//go:build opnative && contracts

package remote

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

type opEngine struct {
	mu     sync.Mutex
	writer *json.Encoder
	reader *json.Decoder
}

func newOPEngine(t *testing.T) *opEngine {
	t.Helper()
	path := os.Getenv("ROSETTA_OP_TEST_ENGINE")
	if path == "" {
		path = "../testutil/opengine/target/debug/rosetta-op-test-engine"
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), path)
	command.Stderr = os.Stderr
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("build native OP test engine with make check-op: %v", err)
	}
	t.Cleanup(func() {
		input.Close()
		io.Copy(io.Discard, output)
		if err := command.Wait(); err != nil && t.Context().Err() == nil {
			t.Error(err)
		}
	})
	return &opEngine{writer: json.NewEncoder(input), reader: json.NewDecoder(output)}
}
func (e *opEngine) request(input any, out any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.writer.Encode(input); err != nil {
		return err
	}
	var response struct {
		Result json.RawMessage
		Error  string
	}
	if err := e.reader.Decode(&response); err != nil {
		return err
	}
	if response.Error != "" {
		return fmt.Errorf("native OP engine: %s", response.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(response.Result, out)
}
