// Package control exposes one running instance to a local operator process.
//
// A private directory holds the Unix socket and the delivery gate. Socket
// access is the authorization boundary: only the owning user can connect. The
// gate is read outside deterministic state transitions and only decides
// whether a ready plan leaves the process.
package control
