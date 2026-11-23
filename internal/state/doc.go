// Package state holds the in-memory pool shallow and deep state together with
// the chain head. It applies V3 events deterministically, tracks which pools
// changed, and supports snapshots plus a bounded multi-block undo history for
// rollback. The package performs no network I/O and reads neither the wall
// clock nor randomness.
package state
