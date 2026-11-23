package feed

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

// FlashClient consumes a raw infrastructure stream, not eth_subscribe RPC.
// Reset runs before each connection attempt so a disconnected prefix cannot
// remain current while the transport reconnects.
type FlashClient struct {
	URL string
	// AllowMissingReceipts transfers receipt completion to the caller.
	AllowMissingReceipts bool
	// OnConnection reports stream availability. Nil disables reporting.
	OnConnection func(connected bool)
}

func (c FlashClient) report(connected bool) {
	if c.OnConnection != nil {
		c.OnConnection(connected)
	}
}

func (c FlashClient) Run(ctx context.Context, reset func() error, handle func(Flashblock) error) error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "wss" && u.Scheme != "ws") {
		return fmt.Errorf("flashblocks: invalid WebSocket URL")
	}
	for ctx.Err() == nil {
		if err := reset(); err != nil {
			return err
		}
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, _, err := websocket.Dial(dialCtx, c.URL, nil)
		cancel()
		if err == nil {
			conn.SetReadLimit(32 << 20)
			c.report(true)
			for {
				readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, data, readErr := conn.Read(readCtx)
				cancel()
				if readErr != nil {
					break
				}
				block, decodeErr := decodeFlashblock(data, !c.AllowMissingReceipts)
				if decodeErr != nil {
					c.report(false)
					conn.CloseNow()
					return decodeErr
				}
				if err := handle(block); err != nil {
					c.report(false)
					conn.CloseNow()
					return err
				}
			}
			c.report(false)
			conn.CloseNow()
		}
		if err := reset(); err != nil {
			return err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
