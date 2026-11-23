package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/Rorical/Rosetta/internal/domain"
)

// headsBackoff bounds reconnect delays. A dropped subscription only costs the
// fast path: the follower still polls the HTTP endpoint.
const (
	headsBackoffMin = 200 * time.Millisecond
	headsBackoffMax = 5 * time.Second
)

// SubscribeHeads calls onHead with each block hash from an eth_subscribe
// "newHeads" WebSocket subscription until ctx ends, reconnecting with backoff.
// A node announces a head after inserting the block, so a read of that hash
// succeeds at once.
func SubscribeHeads(ctx context.Context, url string, onHead func(domain.Hash)) {
	backoff := headsBackoffMin
	for ctx.Err() == nil {
		started := time.Now()
		err := subscribeHeadsOnce(ctx, url, onHead)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = headsBackoffMin
		}
		slog.Warn("head subscription dropped", "error", err, "retry", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, headsBackoffMax)
	}
}

func subscribeHeadsOnce(ctx context.Context, url string, onHead func(domain.Hash)) error {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		// The handshake error repeats the endpoint, which may carry an API key.
		return fmt.Errorf("dial %s: %w", redactURL(url), transportCause(err))
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	if err := conn.Write(ctx, websocket.MessageText, request); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	var subscription string
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var message struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *RPCError       `json:"error"`
			Params struct {
				Subscription string `json:"subscription"`
				Result       struct {
					Hash string `json:"hash"`
				} `json:"result"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		switch {
		case message.ID != nil:
			if message.Error != nil {
				return message.Error
			}
			if err := json.Unmarshal(message.Result, &subscription); err != nil || subscription == "" {
				return errors.New("subscribe: no subscription id")
			}
		case subscription != "" && message.Params.Subscription == subscription:
			hash, err := domain.ParseHash(message.Params.Result.Hash)
			if err != nil {
				return fmt.Errorf("head hash: %w", err)
			}
			onHead(hash)
		}
	}
}
