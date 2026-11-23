package exec

import (
	"context"
	"github.com/Rorical/Rosetta/internal/domain"
	"math/big"
)

// Position is portable execution context. Native consensus/configuration types
// never cross this boundary; receipts retain any backend-specific evidence.
type Position struct {
	Chain     domain.ChainIdentity
	Block     domain.BlockRef
	Timestamp uint64
	BaseFee   *big.Int
	GasLimit  uint64
}

type ConfirmationRequest struct {
	Plan       ExecutorPlan
	Parameters CallParameters
	RawTx      []byte
}

// StateReader binds all reads to one immutable, leased state position.
// Implementations return owned values and honor cancellation.
type StateReader interface {
	Position() Position
	ReadContract(context.Context, domain.Address, []byte) ([]byte, error)
	CodeHash(context.Context, domain.Address) (domain.Hash, error)
	Nonce(context.Context, domain.Address) (uint64, error)
}

// Confirmer performs the complete executor invocation on private state. It may
// not promote simulation writes to the backend's canonical state.
type Confirmer interface {
	Confirm(context.Context, ConfirmationRequest) (*ExecutionEvidence, error)
}

// Snapshot owns a state lease. Retain creates an independently closable lease;
// cancellation alone never authorizes closing a snapshot still in use.
type Snapshot interface {
	StateReader
	Confirmer
	FeeModel() FeeModel
	Retain() Snapshot
	Close()
}

// AssetValuer supplies snapshot-bound conversion evidence rather than a spot
// price detached from the state used for execution.
type AssetValuer interface {
	Conversion(context.Context, domain.Address) (FeeConversion, error)
}

// FeeBudgetEstimator adds state-dependent charges before a route's minProfit
// is fixed. The returned budget must cover the complete unsigned invocation.
// Backends with fees outside execution gas must implement this port.
type FeeBudgetEstimator interface {
	EstimateFeeBudget(context.Context, ExecutorCall) (*big.Int, error)
}
