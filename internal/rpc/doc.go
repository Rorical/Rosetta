// Package rpc is an HTTP JSON-RPC 2.0 client. It owns request encoding,
// batching, timeouts, and retries. It carries no business semantics and never
// signs transactions.
//
// Block and receipt lookups use domain.ErrNotFound for a null node result or a
// block header missing a required field.
package rpc
