package bidmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"time"
)

const MaxArtifactBytes = 8 << 20

type Scope struct {
	ChainID  uint64 `json:"chain_id"`
	CodeHash string `json:"code_hash"`
	Precheck bool   `json:"precheck"`
}

type Node struct {
	Feature   int     `json:"feature"` // -1 is a leaf
	Threshold float64 `json:"threshold"`
	Left      int     `json:"left"`
	Right     int     `json:"right"`
	Value     float64 `json:"value"`
}

type Ensemble struct {
	Trees [][]Node `json:"trees"`
}

// Artifact is a complete frozen release, including its out-of-time acceptance.
// ID is the SHA-256 of the exact loaded bytes, never a mutable filename.
type Artifact struct {
	Schema            string   `json:"schema"`
	Features          []string `json:"features"`
	Scope             Scope    `json:"scope"`
	TrainUntilMS      int64    `json:"train_until_ms"`
	ValidationUntilMS int64    `json:"validation_until_ms"`
	CreatedMS         int64    `json:"created_ms"`
	ExpiresMS         int64    `json:"expires_ms"`
	Approved          bool     `json:"approved"`
	TrainingRows      int      `json:"training_rows"`
	ValidationRows    int      `json:"validation_rows"`
	Low               Features `json:"low"`
	High              Features `json:"high"`
	Win               Ensemble `json:"win"`
	RevertGas         Ensemble `json:"revert_gas"` // expected gas / gas limit
	Delay             Ensemble `json:"delay"`      // decision-to-broadcast p90, milliseconds
	ID                string   `json:"-"`
}

type Prediction struct {
	Win         float64
	RevertRatio float64
	DelayMS     float64
}

func Decode(data []byte, scope Scope) (*Artifact, error) {
	if len(data) > MaxArtifactBytes {
		return nil, fmt.Errorf("bid model exceeds size limit")
	}
	var m Artifact
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode bid model: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("trailing bid model content")
	}
	if scope.ChainID == 0 {
		return nil, fmt.Errorf("missing bid model chain")
	}
	if m.Schema != Schema || !slices.Equal(m.Features, Names) || m.Scope != scope {
		return nil, fmt.Errorf("bid model schema or execution scope mismatch")
	}
	if m.TrainUntilMS <= 0 || m.ValidationUntilMS <= m.TrainUntilMS || m.CreatedMS < m.ValidationUntilMS || m.ExpiresMS <= m.CreatedMS || m.ExpiresMS-m.CreatedMS > int64(24*time.Hour/time.Millisecond) || m.TrainingRows < 1 || m.ValidationRows < 1 {
		return nil, fmt.Errorf("invalid bid model training/validation lifetime")
	}
	if !m.Low.Valid() || !m.High.Valid() {
		return nil, fmt.Errorf("non-finite model support")
	}
	for i := range m.Low {
		if m.Low[i] > m.High[i] {
			return nil, fmt.Errorf("invalid model support range")
		}
	}
	for _, ensemble := range []Ensemble{m.Win, m.RevertGas, m.Delay} {
		if err := ensemble.validate(); err != nil {
			return nil, err
		}
	}
	digest := sha256.Sum256(data)
	m.ID = hex.EncodeToString(digest[:])
	return &m, nil
}

func (e Ensemble) validate() error {
	if len(e.Trees) == 0 || len(e.Trees) > 128 {
		return fmt.Errorf("invalid bid model tree count")
	}
	for _, tree := range e.Trees {
		if len(tree) == 0 || len(tree) > 127 {
			return fmt.Errorf("invalid bid model node count")
		}
		seen := make([]bool, len(tree))
		var visit func(int, int) error
		visit = func(i, depth int) error {
			if i < 0 || i >= len(tree) || seen[i] || depth > 6 {
				return fmt.Errorf("invalid bid model tree topology")
			}
			seen[i] = true
			n := tree[i]
			if math.IsNaN(n.Threshold) || math.IsInf(n.Threshold, 0) || math.IsNaN(n.Value) || math.IsInf(n.Value, 0) {
				return fmt.Errorf("non-finite bid model node")
			}
			if n.Feature == -1 {
				return nil
			}
			if n.Feature < 0 || n.Feature >= FeatureCount {
				return fmt.Errorf("unknown bid model feature")
			}
			if err := visit(n.Left, depth+1); err != nil {
				return err
			}
			return visit(n.Right, depth+1)
		}
		if err := visit(0, 0); err != nil {
			return err
		}
		for _, visited := range seen {
			if !visited {
				return fmt.Errorf("unreachable bid model node")
			}
		}
	}
	return nil
}

func (e Ensemble) predict(f Features) float64 {
	var total float64
	for _, tree := range e.Trees {
		i := 0
		for tree[i].Feature >= 0 {
			n := tree[i]
			if f[n.Feature] <= n.Threshold {
				i = n.Left
			} else {
				i = n.Right
			}
		}
		total += tree[i].Value
	}
	return total
}

func (m *Artifact) Predict(f Features) (Prediction, bool) {
	if !f.Valid() {
		return Prediction{}, false
	}
	for i, v := range f {
		if v < m.Low[i] || v > m.High[i] {
			return Prediction{}, false
		}
	}
	p := Prediction{Win: 1 / (1 + math.Exp(-m.Win.predict(f))), RevertRatio: m.RevertGas.predict(f), DelayMS: m.Delay.predict(f)}
	if math.IsNaN(p.Win) || math.IsNaN(p.RevertRatio) || math.IsNaN(p.DelayMS) || p.RevertRatio <= 0 || p.RevertRatio > 1 || p.DelayMS < 0 || p.DelayMS > 2000 {
		return Prediction{}, false
	}
	return p, true
}
