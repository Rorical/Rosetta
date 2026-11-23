package pool

import "context"

// DeepLoader loads the tick-level state a V3-shaped pool (V3, Slipstream, or
// V4) needs to price a quote. It is a block-level refresh, not a tick-by-tick
// lazy loader: the caller reloads the bitmap words around state.Tick once per
// handled block and keeps the result in the store until the next block.
// Accessors never trigger a load of their own.
//
// block is an eth_call block tag or number. Implementations must honor ctx and
// return an error rather than partial state. They must also set
// DeepState.WindowMin and WindowMax to the inclusive tick range the loaded
// bitmap covers; a zero window means unknown, and the refresh reloads it every
// block until a loader fills it in.
type DeepLoader interface {
	LoadDeep(ctx context.Context, state State, block string, wordRadius int16) (DeepState, error)
}
