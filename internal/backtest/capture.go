package backtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// Record is a portable pool-state/receipt capture. Snapshot records reset state
// on startup, catalog adoption or reorg; block records contain complete receipts.
// It is pool replay data, not an EVM state proof or a message derivation archive.
type Record struct {
	Pools        []pool.State         `json:"pools,omitempty"`
	Deep         []state.DeepDelta    `json:"deep,omitempty"`
	Type         string               `json:"type"`
	ChainID      uint64               `json:"chain_id"`
	Snapshot     json.RawMessage      `json:"snapshot,omitempty"`
	Block        domain.BlockRef      `json:"block"`
	Transactions []ingest.Transaction `json:"transactions,omitempty"`
}
type captureRequest struct {
	pools        []pool.State
	deep         []state.DeepDelta
	dependencies bool
	view         *state.View
	block        domain.BlockRef
	transactions []ingest.Transaction
	next         *os.File
	retired      chan<- *os.File
}
type Capture struct {
	queue   chan captureRequest
	done    chan struct{}
	chainID uint64
	file    *os.File
	mu      sync.Mutex
	err     error
}

func NewCapture(path string, chainID uint64) (*Capture, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	c := &Capture{queue: make(chan captureRequest, 32), done: make(chan struct{}), chainID: chainID, file: file}
	go func() {
		defer close(c.done)
		encoder := json.NewEncoder(file)
		for request := range c.queue {
			if request.next != nil {
				request.retired <- file
				file, encoder = request.next, json.NewEncoder(request.next)
				c.mu.Lock()
				c.file = file
				c.mu.Unlock()
			}
			record := Record{Type: "block", ChainID: chainID, Block: request.block, Transactions: request.transactions}
			if request.dependencies {
				record.Type = "dependencies"
				record.Pools = request.pools
				record.Deep = request.deep
			}
			if request.view != nil {
				record.Type = "snapshot"
				raw, err := request.view.EncodeSnapshotAt(request.block)
				if err != nil {
					c.fail(err)
					return
				}
				record.Snapshot = raw
			}
			if err := encoder.Encode(record); err != nil {
				c.fail(err)
				return
			}
		}
		c.fail(file.Sync())
	}()
	return c, nil
}
func (c *Capture) fail(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = errors.Join(c.err, err)
}
func (c *Capture) enqueue(request captureRequest) error {
	if c == nil {
		return nil
	}
	select {
	case <-c.done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return fmt.Errorf("replay capture stopped: %w", c.err)
	default:
	}
	select {
	case c.queue <- request:
		return nil
	default:
		return fmt.Errorf("replay capture queue full; refusing an incomplete recording")
	}
}
func (c *Capture) Snapshot(view state.View) error {
	return c.enqueue(captureRequest{view: &view, block: view.Block()})
}
func (c *Capture) Block(ref domain.BlockRef, txs []ingest.Transaction) error {
	return c.enqueue(captureRequest{block: ref, transactions: append([]ingest.Transaction(nil), txs...)})
}

// Rotate continues the capture in next after every record already queued, and
// starts next with a snapshot of view so it replays on its own. The returned
// channel yields the file the capture no longer writes; the caller syncs and
// closes it. If the writer stopped before reaching the request, Close hands
// back next instead.
func (c *Capture) Rotate(next *os.File, view state.View) (<-chan *os.File, error) {
	if c == nil {
		return nil, errors.New("no replay capture to rotate")
	}
	retired := make(chan *os.File, 1)
	if err := c.enqueue(captureRequest{next: next, retired: retired, view: &view, block: view.Block()}); err != nil {
		return nil, err
	}
	return retired, nil
}

func (c *Capture) Close() error {
	if c == nil {
		return nil
	}
	close(c.queue)
	<-c.done
	for request := range c.queue {
		if request.next != nil {
			request.retired <- request.next
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return errors.Join(c.err, c.file.Close())
}

func (c *Capture) Dependencies(ref domain.BlockRef, pools []pool.State, deep []state.DeepDelta) error {
	return c.enqueue(captureRequest{block: ref, pools: pools, deep: deep, dependencies: true})
}
