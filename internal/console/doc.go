// Package console is the operator backend behind cmd/rosetta-console.
//
// It runs apart from the trading process and watches one or more instances
// through their private control sockets. It holds no keys, never imports
// internal/exec and reaches a bot only through the gate on the control API.
// Prometheus owns history and alerts; SQLite owns per-opportunity detail that
// is too high-cardinality for metric labels, plus the operator audit log.
package console
