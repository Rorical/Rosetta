package domain

// ChainFamily selects execution semantics, independently of the network ID.
// A family name does not imply that a backend is available in a given build.
type ChainFamily string

const (
	FamilyNitro   ChainFamily = "nitro"
	FamilyOPStack ChainFamily = "op-stack"
)

type ChainIdentity struct {
	Family  ChainFamily
	ChainID uint64
}

// Ordering is how a chain's sequencer orders the transactions it receives.
// It is a property of each deployment: Nitro chains can run either.
type Ordering string

const (
	// OrderingPriority sorts by priority fee within a short round or
	// flashblock, ties by arrival: OP Stack, and Arbitrum One's priority gas
	// auction.
	OrderingPriority Ordering = "priority"
	// OrderingArrival is first come, first served: a tip does not move a
	// transaction ahead (Robinhood Chain).
	OrderingArrival Ordering = "arrival"
)
