package remote

import (
	"sync/atomic"
	"time"
)

// A Nitro sequencer answers eth_sendRawTransaction once the transaction is in
// a block, so the answer time is the time it waited in the arrival-order queue. A send that waited backlogAck landed blocks after its quote; sends
// behind it wait as long and revert the same way, so they are held.
// Answers of a few hundred milliseconds typically land one to three blocks
// after the quote; answers of a second or more land tens of blocks after it.
const (
	backlogAck  = time.Second
	backlogHold = 30 * time.Second
)

// sequencerBacklog holds new sends after a slow sequencer answer. The send
// goroutine writes it and reads it; the zero value holds nothing.
type sequencerBacklog struct{ untilNS atomic.Int64 }

func (b *sequencerBacklog) observe(ack time.Duration, now time.Time) {
	if ack >= backlogAck {
		b.untilNS.Store(now.Add(backlogHold).UnixNano())
	}
}

func (b *sequencerBacklog) held(now time.Time) bool {
	return now.UnixNano() < b.untilNS.Load()
}
