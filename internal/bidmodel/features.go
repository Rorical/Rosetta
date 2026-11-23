// Package bidmodel evaluates bounded, numeric GBDT artifacts locally. Training
// and publishing run outside the trading process.
package bidmodel

import (
	"encoding/json"
	"fmt"
	"math"
)

const Schema = "bid-features-v1"

// Names fixes the Python/Go wire order. Features are captured before selection;
// receipt values and actual broadcast timing are never inference inputs.
var Names = []string{
	"raw_win", "log_tip", "tip_worth", "log_worth", "age_ms", "fast",
	"log_gas_limit", "success_gas_ratio", "revert_gas_ratio", "hops", "funding",
	"first", "unknown", "log_windows", "log_route_windows",
	"v2", "v3", "v4", "camelot_v3", "slipstream", "velodrome_v2", "solidly_v3",
	"log_input", "log_base_fee", "log_extra_fee",
}

const (
	RawWin = iota
	LogTip
	TipWorth
	LogWorth
	AgeMS
	FastLane
	LogGasLimit
	SuccessGasRatio
	RevertGasRatio
	Hops
	Funding
	First
	Unknown
	LogWindows
	LogRouteWindows
	V2
	V3
	V4
	CamelotV3
	Slipstream
	VelodromeV2
	SolidlyV3
	LogInput
	LogBaseFee
	LogExtraFee
	FeatureCount
)

type Features [FeatureCount]float64

// AtTip copies the immutable context and changes only action-dependent inputs.
func (f Features) AtTip(tip, worth uint64, raw float64) Features {
	f[RawWin], f[LogTip], f[TipWorth] = raw, math.Log1p(float64(tip)), float64(tip)/float64(max(worth, 1))
	return f
}

func (f Features) Valid() bool {
	for _, v := range f {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// UnmarshalJSON rejects truncated arrays rather than silently padding support.
func (f *Features) UnmarshalJSON(data []byte) error {
	var values []float64
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	if len(values) != FeatureCount {
		return fmt.Errorf("incorrect bid feature count")
	}
	copy(f[:], values)
	if !f.Valid() {
		return fmt.Errorf("non-finite bid features")
	}
	return nil
}
