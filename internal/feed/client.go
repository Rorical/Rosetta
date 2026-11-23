package feed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/coder/websocket"
)

const (
	defaultReconnectBackoff       = 200 * time.Millisecond
	defaultMaxBackoff             = 5 * time.Second
	defaultReadLimit        int64 = 16 << 20
)

// ClientOptions configures a feed Client. Zero values select the defaults.
type ClientOptions struct {
	ChainID uint64 // Expected chain; zero selects Arbitrum One.
	// NextSequence requests this inclusive position from the standard feed's
	// finite backlog. Nil starts at the server's default live position.
	NextSequence *uint64
	// RequireHeaders requires the pinned v2 handshake and selected chain
	// identity. When false, present headers are still checked.
	RequireHeaders bool
	// ReconnectBackoff is the delay before the first reconnect attempt. It
	// doubles after every attempt that delivers no message.
	ReconnectBackoff time.Duration
	// MaxBackoff caps the reconnect delay.
	MaxBackoff time.Duration
	// ReadLimit is the largest WebSocket message the client accepts.
	ReadLimit int64
	// OnConnection reports stream availability. Nil disables reporting.
	OnConnection func(connected bool)
	// Compress offers permessage-deflate. Robinhood's relay rejects a
	// handshake without it; Arbitrum One's measured feed is uncompressed.
	Compress bool
}

// Client reads frames from one sequencer feed connection and reconnects when
// the connection drops. It does not track sequence continuity; callers see
// decoded messages in arrival order and decide how to recover from gaps.
type Client struct {
	chainID          uint64
	url              string
	reconnectBackoff time.Duration
	maxBackoff       time.Duration
	readLimit        int64
	nextSequence     uint64
	nextKnown        bool
	requireHeaders   bool
	compress         bool
	onConnection     func(bool)
}

// NewClient validates the feed URL and applies option defaults. Both ws and wss
// schemes are accepted; compression is always disabled.
func NewClient(rawURL string, opts ClientOptions) (*Client, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("feed: parse url: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("feed: url scheme %q: want ws or wss", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("feed: url has no host")
	}
	if opts.ReconnectBackoff < 0 || opts.MaxBackoff < 0 || opts.ReadLimit < 0 {
		return nil, errors.New("feed: client options must not be negative")
	}
	if opts.ReconnectBackoff == 0 {
		opts.ReconnectBackoff = defaultReconnectBackoff
	}
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = defaultMaxBackoff
	}
	if opts.ReadLimit == 0 {
		opts.ReadLimit = defaultReadLimit
	}
	if opts.MaxBackoff < opts.ReconnectBackoff {
		return nil, errors.New("feed: max backoff is below reconnect backoff")
	}
	if opts.ChainID == 0 {
		opts.ChainID = domain.ArbitrumOneChainID
	}
	client := &Client{chainID: opts.ChainID,
		url:              rawURL,
		reconnectBackoff: opts.ReconnectBackoff,
		maxBackoff:       opts.MaxBackoff,
		readLimit:        opts.ReadLimit,
		requireHeaders:   opts.RequireHeaders,
		compress:         opts.Compress,
		onConnection:     opts.OnConnection,
	}
	if opts.NextSequence != nil {
		client.nextSequence, client.nextKnown = *opts.NextSequence, true
	}
	return client, nil
}

// Run reads messages from the feed and calls handler for each one in arrival
// order. Dial and read failures are retried with exponential backoff. Run
// returns nil once ctx is done. A handler error stops the client and is
// returned wrapped with the message sequence number.
func (c *Client) Run(ctx context.Context, handler func(Message) error) error {
	delay := c.reconnectBackoff
	for {
		delivered, err := c.consume(ctx, handler)
		var handlerFailure *handlerError
		var protocolFailure *protocolError
		if errors.As(err, &handlerFailure) || errors.As(err, &protocolFailure) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if delivered {
			delay = c.reconnectBackoff
		}
		// The URL is left out; feed URLs may carry credentials in the query.
		slog.Warn("feed: reconnect", "error", err, "delay", delay)
		if !wait(ctx, delay) {
			return nil
		}
		delay = min(delay*2, c.maxBackoff)
	}
}

func (c *Client) report(connected bool) {
	if c.onConnection != nil {
		c.onConnection(connected)
	}
}

// consume dials once and reads until the connection or the handler fails. It
// reports whether any message reached the handler.
func (c *Client) consume(ctx context.Context, handler func(Message) error) (bool, error) {
	headers := http.Header{"Arbitrum-Feed-Client-Version": []string{"2"}}
	if c.nextKnown {
		headers.Set("Arbitrum-Requested-Sequence-Number", strconv.FormatUint(c.nextSequence, 10))
	}
	// Without a required offer, negotiation only adds memory.
	compression := websocket.CompressionDisabled
	if c.compress {
		compression = websocket.CompressionContextTakeover
	}
	conn, response, err := websocket.Dial(ctx, c.url, &websocket.DialOptions{HTTPClient: feedHTTPClient, HTTPHeader: headers, CompressionMode: compression})
	if err != nil {
		return false, fmt.Errorf("feed: dial: %w", err)
	}
	defer conn.CloseNow()
	if err := validateHandshake(response.Header, c.requireHeaders, c.chainID); err != nil {
		return false, err
	}
	conn.SetReadLimit(c.readLimit)
	c.report(true)
	defer c.report(false)

	delivered := false
	for {
		// Read returns only text or binary messages; a frame decodes the same
		// way in either case.
		_, data, err := conn.Read(ctx)
		if err != nil {
			return delivered, fmt.Errorf("feed: read: %w", err)
		}
		frame, err := DecodeFrame(data)
		if err != nil {
			return delivered, err
		}
		for _, message := range frame.Messages {
			if err := handler(message); err != nil {
				return delivered, &handlerError{sequence: message.Sequence, err: err}
			}
			// Advance only a contiguous delivered prefix. Queued messages survive
			// a transport reconnect; gaps must remain requestable from backlog.
			if !c.nextKnown || message.Sequence == c.nextSequence {
				if message.Sequence == ^uint64(0) {
					return delivered, &protocolError{reason: "sequence space exhausted"}
				}
				c.nextSequence, c.nextKnown = message.Sequence+1, true
			}
			delivered = true
		}
	}
}

// wait sleeps for delay or until ctx is done, reporting whether the full delay
// elapsed.
func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// handlerError separates a handler failure, which stops the client, from a
// stream failure, which is retried.
type handlerError struct {
	sequence uint64
	err      error
}

func (e *handlerError) Error() string {
	return fmt.Sprintf("feed: handler for sequence %d: %v", e.sequence, e.err)
}

func (e *handlerError) Unwrap() error { return e.err }

// feedHTTPClient dials IPv4 only, as Nitro's own feed client does. The relay
// limits connections per source address and can answer a client's IPv6
// addresses with 403 once retries pass that limit; a dual-stack dial prefers
// IPv6 and never falls back.
var feedHTTPClient = &http.Client{Transport: ipv4Transport()}

func ipv4Transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network == "tcp" {
			network = "tcp4"
		}
		return dialer.DialContext(ctx, network, address)
	}
	return transport
}
