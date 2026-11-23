package rpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type throttleError struct {
	cause error
	delay time.Duration
}

func (e *throttleError) Error() string { return e.cause.Error() }
func (e *throttleError) Unwrap() error { return e.cause }

// IsRateLimited recognizes provider throttling without retrying unrelated
// -32000 errors such as reverts, missing state or invalid execution contexts.
func IsRateLimited(err error) bool {
	var throttled *throttleError
	if errors.As(err, &throttled) {
		return true
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	message := strings.ToLower(rpcErr.Message)
	if rpcErr.Code == 3 || strings.Contains(message, "execution reverted") {
		return false
	}
	// Alchemy answers throttled batch members with code 429 and "Your app has
	// exceeded its compute units per second capacity".
	return rpcErr.Code == 429 || strings.Contains(message, "rate limit") || strings.Contains(message, "too many requests") || strings.Contains(message, "request limit") || strings.Contains(message, "rps limit") || strings.Contains(message, "compute units per second")
}

// A load-balanced backend whose state trails the requested block answers
// "layer stale" (Alchemy); failing on it would abort a whole reseed.
// Another backend or a moment later serves it.
const (
	staleReadRetries = 3
	staleReadWait    = 250 * time.Millisecond
)

func isStaleBackend(err error) bool {
	var rpcErr *RPCError
	return errors.As(err, &rpcErr) && strings.Contains(strings.ToLower(rpcErr.Message), "layer stale")
}

func retryAfter(header string) time.Duration {
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(header); err == nil {
		return max(time.Second, time.Until(date))
	}
	return time.Second
}

func jsonThrottle(body []byte) error {
	var rows []rpcResponse
	if len(body) > 0 && body[0] == '[' {
		if json.Unmarshal(body, &rows) != nil {
			return nil
		}
	} else {
		var row rpcResponse
		if json.Unmarshal(body, &row) != nil {
			return nil
		}
		rows = []rpcResponse{row}
	}
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		if row.Error == nil || !IsRateLimited(row.Error) {
			return nil
		}
	}
	return &throttleError{cause: rows[0].Error, delay: time.Second}
}

// Only read-only calls can wait indefinitely for provider recovery. Sending a
// transaction retains its bounded attempt policy and cannot be replayed here.
func readOnlyPayload(payload []byte) bool {
	var calls []rpcRequest
	if len(payload) > 0 && payload[0] == '[' {
		if json.Unmarshal(payload, &calls) != nil {
			return false
		}
	} else {
		var call rpcRequest
		if json.Unmarshal(payload, &call) != nil {
			return false
		}
		calls = []rpcRequest{call}
	}
	if len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		if !readOnlyMethod(call.Method) {
			return false
		}
	}
	return true
}

func readOnlyMethod(method string) bool {
	switch method {
	case "eth_call", "eth_chainId", "eth_blockNumber", "debug_traceCall",
		"eth_getBlockByHash", "eth_getBlockByNumber", "eth_getBlockReceipts",
		"eth_getTransactionByHash", "eth_getTransactionReceipt", "eth_getBalance",
		"eth_getTransactionCount", "eth_getCode", "eth_getStorageAt", "eth_getProof", "eth_getLogs":
		return true
	}
	return false
}

func (c *RequestPolicy) coolDown(delay time.Duration) {
	deadline := time.Now().Add(delay).UnixNano()
	for {
		previous := c.throttledUntil.Load()
		if previous >= deadline || c.throttledUntil.CompareAndSwap(previous, deadline) {
			return
		}
	}
}

func httpThrottle(status int, header string, body []byte) error {
	return &throttleError{cause: fmt.Errorf("http %d: %s", status, errorBody(body)), delay: retryAfter(header)}
}

// IsBlockUnavailable recognizes a read at a block the serving backend has not
// reached yet. A provider that balances one endpoint over several nodes
// answers this for a block another node just served; retrying shortly
// succeeds. Missing historical state is a different, lasting error.
func IsBlockUnavailable(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code == 3 {
		return false
	}
	message := strings.ToLower(rpcErr.Message)
	return strings.Contains(message, "ahead of current block") || strings.Contains(message, "header not found") || strings.Contains(message, "unknown block")
}
