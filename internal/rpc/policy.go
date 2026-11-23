package rpc

import (
	"context"
	"sync/atomic"
	"time"
)

// RequestPolicy shares one runtime's external request budget and pacing across
// RPC clients and native-client HTTP transports. It never caches moving heads.
type RequestPolicy struct {
	minInterval    time.Duration
	maxCalls       uint64
	calls          atomic.Uint64
	observe        func(RequestObservation)
	observeResult  func(ResponseObservation)
	pacing         chan struct{}
	nextRequest    time.Time
	throttledUntil atomic.Int64
}

// NewRequestPolicy creates an independent budget; attach it to a runtime context to share it.
func NewRequestPolicy(options Options) *RequestPolicy {
	return &RequestPolicy{minInterval: options.MinInterval, maxCalls: options.MaxCalls, observe: options.Observe, observeResult: options.ObserveResponse, pacing: make(chan struct{}, 1)}
}

type requestPolicyKey struct{}

// WithRequestPolicy binds all participating clients to the same runtime budget.
func WithRequestPolicy(ctx context.Context, policy *RequestPolicy) context.Context {
	return context.WithValue(ctx, requestPolicyKey{}, policy)
}

// RequestPolicyFromContext returns the runtime policy, or nil for an isolated client.
func RequestPolicyFromContext(ctx context.Context) *RequestPolicy {
	policy, _ := ctx.Value(requestPolicyKey{}).(*RequestPolicy)
	return policy
}

type requestObserverKey struct{}

// WithRequestObserver measures one operation without replacing its shared budget
// or budget telemetry. The observer must be safe for concurrent calls.
func WithRequestObserver(ctx context.Context, observe func(RequestObservation)) context.Context {
	return context.WithValue(ctx, requestObserverKey{}, observe)
}

func (p *RequestPolicy) observeRequest(ctx context.Context, observation RequestObservation) {
	observation.Caller = CallerFrom(ctx)
	if p.observe != nil {
		p.observe(observation)
	}
	if observe, _ := ctx.Value(requestObserverKey{}).(func(RequestObservation)); observe != nil {
		observe(observation)
	}
}
