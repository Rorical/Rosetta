package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ErrBlockUnavailable identifies a null block result. Missing required header
// fields remain malformed input and must not be mistaken for a lagging node.
var ErrBlockUnavailable = errors.New("block unavailable")

// Block is the subset of an Arbitrum L2 block header that state tracking needs.
// L1BlockNumber is the L1 block height attached to the header, the same height
// conditional transaction bounds use.
type Block struct {
	Number        uint64
	Hash          domain.Hash
	ParentHash    domain.Hash
	Timestamp     uint64
	L1BlockNumber uint64
}

// BlockByHash returns the header of the block identified by hash. A null
// result or a header missing a required field is reported as an error wrapping
// domain.ErrNotFound.
func (c *Client) BlockByHash(ctx context.Context, hash domain.Hash) (Block, error) {
	block, err := c.blockBy(ctx, "eth_getBlockByHash", []any{hash.Hex(), false}, hash.Hex())
	if err != nil {
		return Block{}, err
	}
	if block.Hash != hash {
		return Block{}, fmt.Errorf("rpc: eth_getBlockByHash %s: node returned hash %s", hash.Hex(), block.Hash.Hex())
	}
	return block, nil
}

// BlockByNumber returns the header of the canonical block at number. A null
// result or a header missing a required field is reported as an error wrapping
// domain.ErrNotFound.
func (c *Client) BlockByNumber(ctx context.Context, number uint64) (Block, error) {
	quantity := hexQuantity(number)
	block, err := c.blockBy(ctx, "eth_getBlockByNumber", []any{quantity, false}, quantity)
	if err != nil {
		return Block{}, err
	}
	if block.Number != number {
		return Block{}, fmt.Errorf("rpc: eth_getBlockByNumber %s: node returned number %d", quantity, block.Number)
	}
	return block, nil
}

// BlockByTag returns the header named by a JSON-RPC block tag, such as
// "latest", "safe", "finalized", "earliest", or "pending". It shares the parse
// path with BlockByNumber but cannot check the returned number because the tag
// does not name one. A null result or a header missing a required field is
// reported as an error wrapping domain.ErrNotFound.
func (c *Client) BlockByTag(ctx context.Context, tag string) (Block, error) {
	if tag == "" {
		return Block{}, errors.New("rpc: eth_getBlockByNumber: empty block tag")
	}
	return c.blockBy(ctx, "eth_getBlockByNumber", []any{tag, false}, tag)
}

// LatestBlock returns the canonical head the node currently reports.
func (c *Client) LatestBlock(ctx context.Context) (Block, error) {
	return c.BlockByTag(ctx, "latest")
}

// blockBy invokes a block lookup method and parses its result. label names the
// requested block in errors.
func (c *Client) blockBy(ctx context.Context, method string, params []any, label string) (Block, error) {
	var result *json.RawMessage
	if err := c.Call(ctx, method, params, &result); err != nil {
		return Block{}, err
	}
	if result == nil {
		return Block{}, fmt.Errorf("rpc: %s %s: %w: %w", method, label, domain.ErrNotFound, ErrBlockUnavailable)
	}

	var payload blockPayload
	if err := json.Unmarshal(*result, &payload); err != nil {
		return Block{}, fmt.Errorf("rpc: %s %s: decode header: %w", method, label, err)
	}
	block, err := payload.block()
	if err != nil {
		return Block{}, fmt.Errorf("rpc: %s %s: %w", method, label, err)
	}
	return block, nil
}

// hexQuantity encodes number as a JSON-RPC quantity with no leading zeros.
func hexQuantity(number uint64) string {
	return "0x" + strconv.FormatUint(number, 16)
}

// blockPayload uses pointers so a missing field is distinguishable from a zero
// value.
type blockPayload struct {
	Number        *string `json:"number"`
	Hash          *string `json:"hash"`
	ParentHash    *string `json:"parentHash"`
	Timestamp     *string `json:"timestamp"`
	L1BlockNumber *string `json:"l1BlockNumber"`
}

func (p blockPayload) block() (Block, error) {
	missing := p.Number == nil || p.Hash == nil || p.ParentHash == nil ||
		p.Timestamp == nil || p.L1BlockNumber == nil
	if missing {
		return Block{}, fmt.Errorf("header is missing a required field: %w", domain.ErrNotFound)
	}

	number, err := parseQuantity("number", *p.Number)
	if err != nil {
		return Block{}, err
	}
	timestamp, err := parseQuantity("timestamp", *p.Timestamp)
	if err != nil {
		return Block{}, err
	}
	l1BlockNumber, err := parseQuantity("l1BlockNumber", *p.L1BlockNumber)
	if err != nil {
		return Block{}, err
	}
	hash, err := domain.ParseHash(*p.Hash)
	if err != nil {
		return Block{}, fmt.Errorf("parse hash: %w", err)
	}
	parentHash, err := domain.ParseHash(*p.ParentHash)
	if err != nil {
		return Block{}, fmt.Errorf("parse parentHash: %w", err)
	}

	return Block{
		Number:        number,
		Hash:          hash,
		ParentHash:    parentHash,
		Timestamp:     timestamp,
		L1BlockNumber: l1BlockNumber,
	}, nil
}

// parseQuantity decodes a 0x-prefixed hex quantity such as "0x1a".
func parseQuantity(field, value string) (uint64, error) {
	raw, ok := strings.CutPrefix(value, "0x")
	if !ok {
		return 0, fmt.Errorf("parse %s %q: missing 0x prefix", field, value)
	}
	number, err := strconv.ParseUint(raw, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", field, value, err)
	}
	return number, nil
}
