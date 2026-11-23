package remote

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

func TestPaperLogsSearchDoesNotScheduleRPCDependencyReads(t *testing.T) {
	for _, mode := range []string{"canonical", "logs"} {
		t.Run(mode, func(t *testing.T) {
			store, pools := poolFixture(t)
			topology, err := pipeline.BuildTopology(t.Context(), pools, pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 8, Notional: func(domain.Address) *big.Int { return big.NewInt(100) }})
			if err != nil {
				t.Fatal(err)
			}
			capture := &captureSearch{}
			version := domain.StateVersion{Source: domain.SourceStandard, Anchor: store.Block()}
			r := &runtime{cfg: config.Config{RPCSearchRefresh: mode}, store: store, worker: &arb.Worker{}, topology: topology, search: pipeline.NewSearchScheduler(capture, 1, nil)}
			if err := r.submit(t.Context(), store.View(), store.View().PoolKeys(), version); err != nil {
				t.Fatal(err)
			}
			if len(capture.jobs) != 1 {
				t.Fatal("missing canonical paper search")
			}
			job := capture.jobs[0]
			if mode == "logs" {
				if job.Prepare != nil || job.PrepareExact != nil || job.Release != nil || job.QuoteBasis != "log_maintained_pool_state" {
					t.Fatal("paper search installed paid dependency callbacks")
				}
				return
			}
			if job.Prepare == nil || job.PrepareExact == nil || job.Release == nil {
				t.Fatal("canonical mode lost exact dependencies")
			}
			job.Release()
		})
	}
}
func TestPaperModeCannotEnableExecutionOrAcceptUnknownPolicy(t *testing.T) {
	base := config.Config{NetworkName: "optimism", PoolConfig: "pools.json", SearchConfig: "search.json", RPCSearchRefresh: "logs"}
	if _, err := (Factory{}).Create(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	base.ExecutionConfig = "execution.json"
	if _, err := (Factory{}).Create(t.Context(), base); err == nil {
		t.Fatal("paper-only dependency policy enabled executor")
	}
	base.ExecutionConfig = ""
	base.RPCSearchRefresh = "invalid"
	if _, err := (Factory{}).Create(t.Context(), base); err == nil {
		t.Fatal("unknown refresh policy accepted")
	}
}
