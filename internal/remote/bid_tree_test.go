package remote

import (
	"encoding/json"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/bidmodel"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/planning"
)

func TestTreeShadowApplyDeadlineAndFallback(t *testing.T) {
	h := newBidHistory()
	h.quiet(unrelated, 100000, 60)
	s := &reactiveRuntime{bids: h.e}
	s.settings.document.BidModel = "apply"
	s.settings.planning.Chain = domain.ChainIdentity{ChainID: 10}
	s.settings.planning.Lifetime = 200 * time.Millisecond
	s.settings.minWin = .2
	request := planning.BidRequest{Route: route, Funding: 1, Amount: big.NewInt(1000000), Surplus: big.NewInt(100000000000), Gas: 1000000, Base: big.NewInt(300), Extra: big.NewInt(1), Ceiling: big.NewInt(100000), ReceivedAt: h.at.Add(-40 * time.Millisecond), Lane: "fast"}
	for _, used := range []uint64{280000, 290000, 300000} {
		s.bids.observeRouteGas(request.Route, request.Funding, used)
	}
	baseline := s.priceBid(h.at, request)
	if baseline.Gas != 300000 || baseline.Learning.Features[bidmodel.SuccessGasRatio] != .3 {
		t.Fatalf("tree features lost the route gas history: %+v", baseline)
	}
	m := bidmodel.Artifact{Schema: bidmodel.Schema, Features: bidmodel.Names, Scope: s.treeScope(), Approved: true, TrainingRows: 800, ValidationRows: 200,
		TrainUntilMS: h.at.Add(-time.Hour).UnixMilli(), ValidationUntilMS: h.at.Add(-time.Minute).UnixMilli(), CreatedMS: h.at.UnixMilli(), ExpiresMS: h.at.Add(time.Hour).UnixMilli()}
	for i := range m.High {
		m.High[i] = 100000
	}
	constant := func(v float64) bidmodel.Ensemble {
		return bidmodel.Ensemble{Trees: [][]bidmodel.Node{{{Feature: -1, Value: v}}}}
	}
	m.Win, m.RevertGas, m.Delay = constant(math.Log(.8/.2)), constant(.15), constant(30)
	load := func() {
		t.Helper()
		path := filepath.Join(t.TempDir(), "tree.json")
		b, _ := json.Marshal(m)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.trees.Load(path, m.Scope, h.at); err != nil {
			t.Fatal(err)
		}
	}
	load()
	s.settings.document.BidTreeMode = "shadow"
	shadow := s.priceBid(h.at, request)
	if shadow.ModelTip.Cmp(baseline.ModelTip) != 0 || shadow.Expected.Cmp(baseline.Expected) != 0 || shadow.Win != baseline.Win || shadow.Learning.Applied || shadow.Learning.TreeRevertGas != 150000 {
		t.Fatalf("shadow changed action: %+v", shadow)
	}
	s.settings.document.BidTreeMode = "apply"
	applied := s.priceBid(h.at, request)
	if !applied.Learning.Applied || applied.Win != .8 || applied.RevertGas != 150000 || applied.Learning.Features[4] != 40 || applied.Learning.ModelID == "" {
		t.Fatalf("tree not applied: %+v", applied)
	}
	m.Delay = constant(180)
	load()
	if late := s.priceBid(h.at, request); late.Reason == "" {
		t.Fatal("predicted-expired opportunity accepted")
	}
	m.High[3] = 0
	load()
	fallback := s.priceBid(h.at, request)
	if fallback.Learning.Applied || fallback.Learning.Status != "outside_support" {
		t.Fatal("unsupported tree action used")
	}
	if fallback.Win != planning.Calibrate(fallback.WinRaw, fallback.Shift) {
		t.Fatal("fallback bypassed baseline")
	}
}
