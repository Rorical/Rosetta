package feed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// FastClient authenticates using a bearer key held only in memory. It has no
// replay facility. Handlers must enqueue quickly; downstream overload must
// invalidate its branch rather than silently dropping transactions.
type FastClient struct {
	transport *Client
	key       string
}

func NewFastClient(rawURL, key string, opts ClientOptions) (*FastClient, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("fast feed: use a ws/wss URL without credentials, query or fragment")
	}
	if key == "" || strings.ContainsAny(key, " \r\n\t") {
		return nil, errors.New("fast feed: missing or invalid bearer key")
	}
	transport, err := NewClient(rawURL, opts)
	if err != nil {
		return nil, errors.New("fast feed: invalid URL or client options")
	}
	return &FastClient{transport: transport, key: key}, nil
}

// Run calls invalidate synchronously before connecting and after every stream
// loss, including cancellation and handler failure. No subsequent message can
// reach handler until invalidation has completed. HTTP 401/429 stop the client;
// network and malformed-frame failures reconnect with bounded backoff.
func (c *FastClient) Run(ctx context.Context, handler func(FastMessage) error, invalidate func() error) error {
	if handler == nil || invalidate == nil {
		return errors.New("fast feed: message and invalidation handlers are required")
	}
	if err := invalidate(); err != nil {
		return fmt.Errorf("fast feed: invalidate: %w", err)
	}
	delay := c.transport.reconnectBackoff
	for ctx.Err() == nil {
		delivered, err := c.consume(ctx, handler)
		resetErr := invalidate()
		if resetErr != nil {
			return errors.Join(err, fmt.Errorf("fast feed: invalidate: %w", resetErr))
		}
		if ctx.Err() != nil {
			return nil
		}
		var handlerFailure *handlerError
		var rejected *fastRejected
		var protocol *protocolError
		if errors.As(err, &handlerFailure) || errors.As(err, &rejected) || errors.As(err, &protocol) {
			return err
		}
		if delivered {
			delay = c.transport.reconnectBackoff
		}
		if !wait(ctx, delay) {
			return nil
		}
		delay = min(delay*2, c.transport.maxBackoff)
	}
	return nil
}

func (c *FastClient) consume(ctx context.Context, handler func(FastMessage) error) (bool, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, response, err := websocket.Dial(dialCtx, c.transport.url, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + c.key}},
		CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err != nil {
		if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusTooManyRequests) {
			return false, &fastRejected{status: response.StatusCode}
		}
		// Dial errors may contain URLs and server-controlled text. Do not expose
		// either, as authentication services can echo the request credentials.
		return false, errors.New("fast feed: connection failed")
	}
	defer conn.CloseNow()
	if err := validateHandshake(response.Header, false, c.transport.chainID); err != nil {
		return false, err
	}
	conn.SetReadLimit(c.transport.readLimit)
	c.transport.report(true)
	defer c.transport.report(false)
	delivered := false
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return delivered, errors.New("fast feed: stream closed")
		}
		if kind != websocket.MessageBinary {
			return delivered, errors.New("fast feed: expected binary frame")
		}
		message, err := DecodeFastMessage(data)
		if err != nil {
			return delivered, err
		}
		if err := handler(message); err != nil {
			return delivered, &handlerError{sequence: message.TxIndex, err: err}
		}
		delivered = true
	}
}

type fastRejected struct{ status int }

func (e *fastRejected) Error() string {
	return fmt.Sprintf("fast feed: connection rejected (HTTP %d)", e.status)
}
