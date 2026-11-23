package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// maxBatchCalls bounds one JSON-RPC batch. The public Arbitrum endpoint started
// returning 429 when startup bootstrap sent every read in one request, so the
// harness keeps each batch small.
const maxBatchCalls = 50

// rpcCaller adapts the typed RPC client to the loader's caller interfaces.
type rpcCaller struct {
	client *rpc.Client
}

var (
	_ pool.ContractCaller      = rpcCaller{}
	_ pool.BatchContractCaller = rpcCaller{}
)

// CallContract reads to at block and writes the 0x-prefixed hex result to out.
func (c rpcCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	params := []any{
		map[string]string{"to": to.Hex(), "data": "0x" + hex.EncodeToString(data)},
		block,
	}
	var raw string
	if err := c.client.Call(ctx, "eth_call", params, &raw); err != nil {
		return fmt.Errorf("eth_call %s: %w", to, err)
	}
	*out = raw
	return nil
}

// CallContracts performs the calls in consecutive JSON-RPC batches. Results
// keep the call order.
func (c rpcCaller) CallContracts(ctx context.Context, calls []pool.ContractCall) ([]string, error) {
	if len(calls) == 0 {
		return []string{}, nil
	}
	out := make([]string, len(calls))
	for start := 0; start < len(calls); start += maxBatchCalls {
		end := min(start+maxBatchCalls, len(calls))
		if err := c.callBatch(ctx, calls[start:end], out[start:end]); err != nil {
			return nil, fmt.Errorf("eth_call batch calls %d-%d: %w", start, end-1, err)
		}
	}
	return out, nil
}

func (c rpcCaller) callBatch(ctx context.Context, calls []pool.ContractCall, out []string) error {
	batch := make([]rpc.Call, len(calls))
	for i, call := range calls {
		batch[i] = rpc.Call{
			Method: "eth_call",
			Params: []any{
				map[string]string{"to": call.To.Hex(), "data": "0x" + hex.EncodeToString(call.Data)},
				call.Block,
			},
		}
	}
	results, err := c.client.Batch(ctx, batch)
	if err != nil {
		return fmt.Errorf("eth_call: %w", err)
	}
	for i, result := range results {
		if result.Error != nil {
			return fmt.Errorf("eth_call %s call %d: %w", calls[i].To, i, result.Error)
		}
		if err := json.Unmarshal(result.Result, &out[i]); err != nil {
			return fmt.Errorf("eth_call %s call %d: decode result: %w", calls[i].To, i, err)
		}
	}
	return nil
}
