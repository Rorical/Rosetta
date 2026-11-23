// Package arb evaluates cyclic arbitrage paths over a precomputed cycle table.
// It quotes every leg through an injected Quoter against a read-only state
// view, refines two-pool cycles with the constant-liquidity closed-form input,
// and reports gross and gas-adjusted net profit. It does not sign or send
// transactions.
package arb
