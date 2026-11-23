// Package config defines command-line settings for the L2 RPC backend.
package config

import "time"

// Config selects source state, feed inputs, search policy and runtime outputs.
type Config struct {
	TokenRiskFile     string
	PoolRiskFile      string
	ReactivePreflight bool
	ReactiveConfig    string
	RPCSearchRefresh  string
	ControlDir        string
	RunRoot           string
	ReplayOut         string
	EventSource       string
	FlashblocksURL    string
	SubmitURL         string
	PoolConfig        string
	PoolCatalog       string
	RPCPoll           time.Duration
	// RPCHeadsURL is a WebSocket endpoint whose newHeads subscription wakes
	// the follower with each block hash as soon as the node inserts it.
	RPCHeadsURL             string
	RPCMaxCalls             uint64
	FlashblocksReceiptsRPC  bool
	RPCMinInterval          time.Duration
	NetworkName             string
	NetworkFile             string
	Network                 Network
	FeedURL                 string
	RPCURL                  string
	Blocks                  int
	JSONOutput              bool
	PoolSnapshot            string
	PoolSnapshotOut         string
	FastURL                 string
	FastFirstIndex          int64
	FastCanonicalFirstIndex int64
	SearchConfig            string
	CandidateOut            string
	ResearchConfig          string
	ResearchOut             string
	DiscoveryConfig         string
	ExecutionPaperConfig    string
	ExecutionConfig         string
	PlanOut                 string
	MetricsListen           string
	ProfileListen           string
	MetricsOut              string
	RPCWait                 time.Duration
}
