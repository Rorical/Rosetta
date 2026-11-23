package telemetry

import "github.com/Rorical/Rosetta/internal/rpc"

// ObserveRPC counts all shared-policy external attempts, including batch members.
func (r *Registry) ObserveRPC(observation rpc.RequestObservation) {
	r.Add(Standard, RPCHTTPAttempts, 1)
	r.Add(Standard, RPCMethodCalls, observation.Calls)
	if r != nil && observation.Caller < rpc.CallerCount {
		r.rpcRequests[observation.Caller].Add(1)
		r.rpcCalls[observation.Caller].Add(observation.Calls)
	}
	if observation.Retry {
		r.Add(Standard, RPCRetries, 1)
	}
}

// ObserveRPCResponse records provider health. Methods fold into fixed stages so
// the label set stays bounded whatever a provider or caller invents.
func (r *Registry) ObserveRPCResponse(response rpc.ResponseObservation) {
	if r == nil {
		return
	}
	r.Observe(Standard, rpcStage(response.Method), response.Elapsed)
	if response.Failed {
		r.Add(Standard, RPCHTTPErrors, 1)
	}
	if response.Throttled {
		r.Add(Standard, RPCThrottled, 1)
	}
}

func rpcStage(method string) Stage {
	switch method {
	case "eth_getBlockByNumber", "eth_getBlockByHash":
		return RPCBlock
	case "eth_getBlockReceipts", "eth_getTransactionReceipt":
		return RPCReceipts
	case "eth_getLogs":
		return RPCLogs
	case "eth_call", "eth_estimateGas", "debug_traceCall":
		return RPCCall
	case rpc.BatchMethod:
		return RPCBatch
	default:
		return RPCOther
	}
}
