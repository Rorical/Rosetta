package pool

import (
	"context"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ContractCaller performs a read-only eth_call against a contract.
//
// Implementations must honor ctx and must not mutate to or data. out receives
// the 0x-prefixed hex encoding of the returned bytes. block is an eth_call
// block tag or number.
type ContractCaller interface {
	CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error
}

// ContractCall is one read-only eth_call in a batch. Block is the same eth_call
// block tag or number accepted by ContractCaller.
type ContractCall struct {
	To    domain.Address
	Data  []byte
	Block string
}

// BatchContractCaller performs read-only eth_calls in one request.
//
// The returned slice has one 0x-prefixed hex result per call, in the same order
// as calls. Implementations must honor ctx and must not mutate calls or their
// Data.
type BatchContractCaller interface {
	CallContracts(ctx context.Context, calls []ContractCall) ([]string, error)
}
