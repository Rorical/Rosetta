package exec

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// SubmitPath is one endpoint that receives every signed transaction. Name is
// what records show; it must not carry credentials.
type SubmitPath struct {
	Name   string
	Client *rpc.Client
}

// PathResult is one endpoint's answer to a fanned-out send.
type PathResult struct {
	Name      string `json:"name"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Accepted  bool   `json:"accepted"`
	Error     string `json:"error,omitempty"`
}

// fanout sends the same signed bytes to every path and returns the first
// acknowledgement. Identical bytes have one hash, so at most one copy can land;
// the others are answered as known or as a used nonce. Slower paths finish in
// the background and are reported together, to compare which path is faster.
func (n RPCDispatchNetwork) fanout(ctx context.Context, raw []byte) (domain.Hash, error) {
	paths := append([]SubmitPath{{Name: "primary", Client: n.Submit}}, n.Fanout...)
	type answer struct {
		index int
		hash  domain.Hash
		err   error
		at    time.Time
	}
	started := time.Now()
	answers := make(chan answer, len(paths))
	// Sends outlive the caller's return: a slower path still delivers.
	sendCtx := context.WithoutCancel(ctx)
	payload := []any{"0x" + hex.EncodeToString(raw)}
	for i, path := range paths {
		go func() {
			var text string
			err := path.Client.Call(sendCtx, "eth_sendRawTransaction", payload, &text)
			var hash domain.Hash
			if err == nil {
				hash, err = domain.ParseHash(text)
			}
			answers <- answer{index: i, hash: hash, err: err, at: time.Now()}
		}()
	}
	results := make([]PathResult, len(paths))
	var first *answer
	var firstErr error
	received := 0
	finish := func() {
		for received < len(paths) {
			a := <-answers
			received++
			results[a.index] = pathResult(paths[a.index].Name, started, a.at, a.err)
		}
		if n.OnPaths != nil {
			n.OnPaths(first.hash, results)
		}
	}
	for received < len(paths) {
		a := <-answers
		received++
		results[a.index] = pathResult(paths[a.index].Name, started, a.at, a.err)
		if a.err == nil {
			first = &a
			go finish()
			return a.hash, nil
		}
		if firstErr == nil || a.index == 0 {
			firstErr = a.err
		}
	}
	if n.OnPaths != nil {
		n.OnPaths(domain.Hash{}, results)
	}
	return domain.Hash{}, firstErr
}

func pathResult(name string, started, at time.Time, err error) PathResult {
	result := PathResult{Name: name, ElapsedMS: at.Sub(started).Milliseconds(), Accepted: err == nil}
	if err != nil {
		result.Error = sendError(err)
	}
	return result
}

// sendError keeps the node's JSON-RPC answer and drops transport details,
// whose URLs can embed endpoint credentials.
func sendError(err error) string {
	var rpcErr *rpc.RPCError
	if errors.As(err, &rpcErr) {
		return fmt.Sprintf("rpc %d: %s", rpcErr.Code, rpcErr.Message)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "transport error"
}
