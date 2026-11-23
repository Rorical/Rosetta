// Package backtest replays recorded blocks through the state store and the
// arbitrage engine, producing the opportunities found at each block. Dataset
// loading and result accounting are supplied by callers; this package owns the
// replay loop.
package backtest
