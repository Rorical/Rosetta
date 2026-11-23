package rpc

import "context"

// Caller names the component that issued a request. The set is fixed so a
// metric label built from it stays bounded.
type Caller uint8

const (
	CallerOther Caller = iota
	CallerBootstrap
	CallerFollower
	CallerFlashReceipts
	CallerReactiveRefresh
	CallerTick
	CallerDiscovery
	CallerConfirmation
	CallerPaper
	CallerResearch
	CallerSettlement
	CallerCount
)

var callerNames = [CallerCount]string{"other", "bootstrap", "follower", "flash_receipts", "reactive_refresh", "tick", "discovery", "confirmation", "paper", "research", "settlement"}

func (c Caller) String() string {
	if c >= CallerCount {
		return callerNames[CallerOther]
	}
	return callerNames[c]
}

type callerKey struct{}

// WithCaller attributes every request made under ctx to caller.
func WithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

// CallerFrom returns the caller bound to ctx, or CallerOther.
func CallerFrom(ctx context.Context) Caller {
	caller, _ := ctx.Value(callerKey{}).(Caller)
	return caller
}
