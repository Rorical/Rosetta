package bidmodel

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fixture(now time.Time) Artifact {
	m := Artifact{Schema: Schema, Features: Names, Scope: Scope{ChainID: 10, CodeHash: "test"},
		TrainUntilMS: now.Add(-time.Hour).UnixMilli(), ValidationUntilMS: now.Add(-time.Minute).UnixMilli(), CreatedMS: now.UnixMilli(), ExpiresMS: now.Add(time.Hour).UnixMilli(),
		Approved: true, TrainingRows: 800, ValidationRows: 200}
	for i := range m.High {
		m.High[i] = 100
	}
	m.Win = Ensemble{Trees: [][]Node{{{Feature: 1, Threshold: 2, Left: 1, Right: 2}, {Feature: -1, Value: 0}, {Feature: -1, Value: 1}}}}
	m.RevertGas = Ensemble{Trees: [][]Node{{{Feature: -1, Value: .05}}}}
	m.Delay = Ensemble{Trees: [][]Node{{{Feature: -1, Value: 40}}}}
	return m
}

func TestModelRejectsMalformedArtifacts(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name  string
		alter func(*Artifact)
	}{
		{"cycle", func(m *Artifact) { m.Win.Trees[0][0].Left = 0 }},
		{"feature", func(m *Artifact) { m.Win.Trees[0][0].Feature = FeatureCount }},
		{"scope", func(m *Artifact) { m.Scope.ChainID = 1 }},
		{"schema", func(m *Artifact) { m.Schema = "unknown" }},
		{"range", func(m *Artifact) { m.Low[0] = 101 }},
		{"time", func(m *Artifact) { m.ValidationUntilMS = m.TrainUntilMS }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := fixture(now)
			test.alter(&m)
			data, _ := json.Marshal(m)
			if _, err := Decode(data, fixture(now).Scope); err == nil {
				t.Fatal("accepted malformed model")
			}
		})
	}
	var f Features
	if json.Unmarshal([]byte(`[1]`), &f) == nil {
		t.Fatal("accepted truncated features")
	}
}

func TestAtomicReloadFallbackAndExpiry(t *testing.T) {
	now := time.Now()
	m := fixture(now)
	path := filepath.Join(t.TempDir(), "model.json")
	data, _ := json.Marshal(m)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	var store Store
	if err := store.Load(path, m.Scope, now); err != nil {
		t.Fatal(err)
	}
	pinned, _ := store.Snapshot(now, true)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				current, _ := store.Snapshot(now, true)
				if current != nil {
					current.Predict(Features{1: 3})
				}
			}
		}()
	}
	if err := os.WriteFile(path, []byte(`{"partial":`), 0600); err != nil {
		t.Fatal(err)
	}
	if store.Load(path, m.Scope, now) == nil {
		t.Fatal("accepted torn write")
	}
	retained, _ := store.Snapshot(now, true)
	if retained != pinned {
		t.Fatal("invalid release replaced last valid model")
	}
	if model, reason := store.Snapshot(now.Add(2*time.Hour), true); model != nil || reason != "model_expired" {
		t.Fatal("expired model still available")
	}
	m.Approved = false
	data, _ = json.Marshal(m)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Load(path, m.Scope, now); err != nil {
		t.Fatal(err)
	}
	if model, reason := store.Snapshot(now, true); model != nil || reason != "model_unapproved" {
		t.Fatal("unapproved model applied")
	}
	if model, _ := store.Snapshot(now, false); model == nil {
		t.Fatal("shadow cannot evaluate candidate")
	}
	workers.Wait()
	if p, ok := pinned.Predict(Features{1: 2}); !ok || p.Win != .5 || p.DelayMS != 40 {
		t.Fatalf("pinned prediction changed: %+v", p)
	}
	if _, ok := pinned.Predict(Features{1: 101}); ok {
		t.Fatal("unsupported action accepted")
	}
}

func BenchmarkPredict(b *testing.B) {
	m := fixture(time.Now())
	for len(m.Win.Trees) < 64 {
		m.Win.Trees = append(m.Win.Trees, m.Win.Trees[0])
	}
	for b.Loop() {
		m.Predict(Features{1: 3})
	}
}

// The fixture carries independent predictions from LightGBM, not values
// recomputed with the portable evaluator under test.
func TestLightGBMReferenceVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/lightgbm-4.6.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Artifact json.RawMessage `json:"artifact"`
		Cases    []struct {
			Features Features `json:"features"`
			Win      float64  `json:"win"`
			Revert   float64  `json:"revert_ratio"`
			Delay    float64  `json:"delay_ms"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	model, err := Decode(fixture.Artifact, Scope{ChainID: 10, CodeHash: "0x1111111111111111111111111111111111111111111111111111111111111111", Precheck: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, vector := range fixture.Cases {
		p, ok := model.Predict(vector.Features)
		if !ok || math.Abs(p.Win-vector.Win) > 1e-12 || math.Abs(p.RevertRatio-vector.Revert) > 1e-12 || math.Abs(p.DelayMS-vector.Delay) > 1e-12 {
			t.Fatalf("reference mismatch: %+v", p)
		}
	}
}
