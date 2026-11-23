// Package ingest applies receipt logs of tracked pools to pool state. A
// TransactionBranch stages one canonical block over the accepted store; Fast
// and log branches overlay tentative prefixes from preconfirmation streams and
// reconcile them against canonical blocks. It performs no network I/O, does
// not decode L2 batch or signed-transaction payloads, and does not search
// paths.
package ingest
