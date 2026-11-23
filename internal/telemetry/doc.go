// Package telemetry collects bounded pipeline counters and latency histograms.
// Labels are fixed at compile time; no transaction or configuration data is
// retained. Observation has no effect on execution or state decisions.
package telemetry
