// Package feed reads frames from the Arbitrum sequencer feed over WebSocket and
// decodes them. It owns transport, reconnect handling, and frame parsing; it
// never reads chain state and never makes trading decisions. Callers receive
// decoded messages and decide how to recover from sequence gaps.
//
// The decoder and client are experimental, independently written
// implementations of the public wire format. They use no Nitro source code.
package feed
