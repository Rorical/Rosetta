package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const maxErrorBody = 512

// Call is one JSON-RPC method invocation for Batch.
type Call struct {
	Method string
	Params any
}

// RPCError is a JSON-RPC error object returned by the server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements error.
func (e *RPCError) Error() string {
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return fmt.Sprintf("rpc: error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("rpc: error %d: %s: %s", e.Code, e.Message, e.Data)
}

// Result is one batch result. Exactly one of Result and Error is normally set.
type Result struct {
	Result json.RawMessage
	Error  *RPCError
}

// Options configures a Client. Timeout is the per-request HTTP timeout; zero
// means no client timeout. MaxRetries is the number of retries after the first
// attempt; zero disables retries. Backoff is the wait between attempts.
type Options struct {
	// RetryReads waits for transient provider recovery on read-only methods until context cancellation or budget exhaustion.
	RetryReads bool
	Timeout    time.Duration
	MaxRetries int
	Backoff    time.Duration
	MaxCalls   uint64                   // Zero disables the isolated-client request-unit budget.
	Observe    func(RequestObservation) // Called per HTTP attempt; must be concurrency-safe.
	// ObserveResponse is called after each attempt; must be concurrency-safe.
	ObserveResponse func(ResponseObservation)
	MinInterval     time.Duration // Pacing interval per method; batches count every member, including retries.
	// Isolated ignores a runtime policy bound to the context. Submission
	// endpoints use it so read pacing and read throttles never delay a send.
	Isolated bool
	// DialAddress, when set, is the host:port every connection dials instead
	// of the URL's host; TLS verification and the Host header still use the
	// URL's name. Submission uses it to reach each address behind one name.
	DialAddress string
}

// Client is a JSON-RPC client. It is safe for concurrent use.
type Client struct {
	retryReads bool
	isolated   bool
	url        string
	httpClient *http.Client
	maxRetries int
	backoff    time.Duration
	nextID     atomic.Uint64
	policy     *RequestPolicy
}

// NewClient returns a client for the given HTTP endpoint. It performs no
// network I/O.
func NewClient(url string, opts Options) *Client {
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	if opts.Backoff < 0 {
		opts.Backoff = 0
	}
	httpClient := &http.Client{Timeout: opts.Timeout}
	if opts.DialAddress != "" {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, opts.DialAddress)
		}
		httpClient.Transport = transport
	}
	return &Client{
		url:        url,
		retryReads: opts.RetryReads,
		isolated:   opts.Isolated,
		policy:     NewRequestPolicy(opts),
		httpClient: httpClient,
		maxRetries: opts.MaxRetries,
		backoff:    opts.Backoff,
	}
}

// Call invokes method and unmarshals the result into out. A nil out discards
// the result. JSON-RPC errors are returned as *RPCError and are not retried.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	request := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextID.Add(1),
		Method:  method,
		Params:  params,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("rpc: %s: encode request: %w", method, err)
	}

	body, err := c.post(ctx, method, payload, 1)
	if err != nil {
		return fmt.Errorf("rpc: %s: %w", method, err)
	}

	var response rpcResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("rpc: %s: decode response: %w", method, err)
	}
	if response.Error != nil {
		return response.Error
	}
	if out == nil {
		return nil
	}
	if len(response.Result) == 0 {
		return fmt.Errorf("rpc: %s: response has no result", method)
	}
	if err := json.Unmarshal(response.Result, out); err != nil {
		return fmt.Errorf("rpc: %s: decode result: %w", method, err)
	}
	return nil
}

// Batch sends all calls in one HTTP request. Results are returned in the same
// order as calls, regardless of response order. Per-call JSON-RPC errors are
// stored in Result.Error; the returned error reports transport or protocol
// failures.
//
// A provider can throttle some members of a batch and answer the rest. With
// read recovery on, throttled read-only members are sent again, alone, after a
// growing cool-down, until they are answered or ctx ends.
func (c *Client) Batch(ctx context.Context, calls []Call) ([]Result, error) {
	results, err := c.batchOnce(ctx, calls)
	if err != nil || !c.retryReads {
		return results, err
	}
	stale := 0
	for attempt := 1; ; attempt++ {
		var throttled []int
		limited := false
		for i, result := range results {
			if result.Error == nil || !readOnlyMethod(calls[i].Method) {
				continue
			}
			if IsRateLimited(result.Error) {
				throttled, limited = append(throttled, i), true
			} else if isStaleBackend(result.Error) && stale < staleReadRetries {
				throttled = append(throttled, i)
			}
		}
		if len(throttled) == 0 {
			return results, nil
		}
		if limited {
			c.requestPolicy(ctx).coolDown(time.Duration(min(attempt, 30)) * time.Second)
		} else {
			// Not a throttle: the shared policy keeps its pace.
			stale++
			select {
			case <-ctx.Done():
				return results, ctx.Err()
			case <-time.After(time.Duration(stale) * staleReadWait):
			}
		}
		again := make([]Call, len(throttled))
		for j, i := range throttled {
			again[j] = calls[i]
		}
		retried, err := c.batchOnce(ctx, again)
		if err != nil {
			return results, err
		}
		for j, i := range throttled {
			results[i] = retried[j]
		}
	}
}

func (c *Client) batchOnce(ctx context.Context, calls []Call) ([]Result, error) {
	if len(calls) == 0 {
		return []Result{}, nil
	}

	firstID := c.nextID.Add(uint64(len(calls))) - uint64(len(calls)) + 1
	requests := make([]rpcRequest, len(calls))
	for i, call := range calls {
		requests[i] = rpcRequest{
			JSONRPC: "2.0",
			ID:      firstID + uint64(i),
			Method:  call.Method,
			Params:  call.Params,
		}
	}
	payload, err := json.Marshal(requests)
	if err != nil {
		return nil, fmt.Errorf("rpc: batch: encode request: %w", err)
	}

	body, err := c.post(ctx, BatchMethod, payload, uint64(len(calls)))
	if err != nil {
		return nil, fmt.Errorf("rpc: batch: %w", err)
	}

	var responses []rpcResponse
	if err := json.Unmarshal(body, &responses); err != nil {
		return nil, fmt.Errorf("rpc: batch: decode response: %w", err)
	}

	results := make([]Result, len(calls))
	seen := make([]bool, len(calls))
	for _, response := range responses {
		var id uint64
		if err := json.Unmarshal(response.ID, &id); err != nil {
			return nil, fmt.Errorf("rpc: batch: decode response id: %w", err)
		}
		if id < firstID || id-firstID >= uint64(len(calls)) {
			return nil, fmt.Errorf("rpc: batch: response id %d is outside the request range", id)
		}
		index := int(id - firstID)
		if seen[index] {
			return nil, fmt.Errorf("rpc: batch: duplicate response id %d", id)
		}
		seen[index] = true
		results[index] = Result{Result: response.Result, Error: response.Error}
		if response.Error != nil && IsRateLimited(response.Error) {
			c.requestPolicy(ctx).coolDown(time.Second)
		}
	}
	for i, found := range seen {
		if !found {
			return nil, fmt.Errorf("rpc: batch: no response for call %d (id %d)", i, firstID+uint64(i))
		}
	}
	return results, nil
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

// post retries transient transport/provider failures under the shared budget.
// Read recovery uses bounded backoff; retries keep the same request IDs.
// requestPolicy is the runtime policy bound to ctx unless this client is
// isolated or none is bound.
func (c *Client) requestPolicy(ctx context.Context) *RequestPolicy {
	if shared := RequestPolicyFromContext(ctx); shared != nil && !c.isolated {
		return shared
	}
	return c.policy
}

func (c *Client) post(ctx context.Context, method string, payload []byte, calls uint64) ([]byte, error) {
	policy := c.requestPolicy(ctx)
	attempts := c.maxRetries + 1
	var lastErr error
	recoverReads := c.retryReads && readOnlyPayload(payload)
	for attempt := 0; attempt < attempts || recoverReads; attempt++ {
		if attempt > 0 {
			delay := c.backoff
			if recoverReads {
				delay = min(30*time.Second, max(delay, time.Duration(min(attempt, 30))*time.Second))
			}
			if err := wait(ctx, delay); err != nil {
				return nil, err
			}
		}
		if err := policy.pace(ctx, calls); err != nil {
			return nil, err
		}
		if err := policy.reserveCalls(ctx, calls); err != nil {
			return nil, err
		}
		policy.observeRequest(ctx, RequestObservation{Calls: calls, Retry: attempt > 0})
		started := time.Now()
		body, retryable, err := c.postOnce(ctx, payload)
		var throttle *throttleError
		throttled := errors.As(err, &throttle)
		ctxErr := ctx.Err()
		if policy.observeResult != nil && (err == nil || ctxErr == nil) {
			policy.observeResult(ResponseObservation{Method: method, Elapsed: time.Since(started), Failed: err != nil, Throttled: throttled})
		}
		if err == nil {
			return body, nil
		}
		if ctxErr != nil {
			return nil, ctxErr
		}
		if !retryable {
			return nil, err
		}
		if throttled {
			policy.coolDown(max(throttle.delay, time.Duration(min(attempt+1, 30))*time.Second))
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%w (after %d attempts)", lastErr, attempts)
}

func (c *Client) postOnce(ctx context.Context, payload []byte) ([]byte, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("create request for %s: %w", redactURL(c.url), transportCause(err))
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		// http.Client.Do wraps transport failures in *url.Error, whose message
		// contains the whole endpoint. Provider keys often live in the path or
		// query, so keep only the scheme and host and drop that wrapper.
		return nil, true, fmt.Errorf("post %s: %w", redactURL(c.url), transportCause(err))
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, true, fmt.Errorf("read response: %w", err)
	}

	if response.StatusCode == http.StatusTooManyRequests {
		return nil, true, httpThrottle(response.StatusCode, response.Header.Get("Retry-After"), body)
	}
	if response.StatusCode >= 500 {
		return nil, true, fmt.Errorf("http %d: %s", response.StatusCode, errorBody(body))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, false, fmt.Errorf("http %d: %s", response.StatusCode, errorBody(body))
	}
	if err := jsonThrottle(body); err != nil {
		return nil, true, err
	}
	return body, false, nil
}

// redactURL returns the scheme and host of rawURL with userinfo, path, query,
// and fragment removed. RPC errors are logged, and HTTP endpoints commonly
// carry the provider API key in the path or query, so no other part may reach
// an error message. An unparseable endpoint is replaced entirely.
func redactURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<rpc endpoint>"
	}
	return parsed.Scheme + "://" + parsed.Host
}

// transportCause unwraps the *url.Error http.Client.Do adds so its message,
// which repeats the full endpoint, is not printed. The underlying cause is
// preserved for errors.Is and errors.As.
func transportCause(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func errorBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(empty body)"
	}
	if len(text) > maxErrorBody {
		return text[:maxErrorBody] + "..."
	}
	return text
}
