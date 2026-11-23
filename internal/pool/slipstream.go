package pool

import (
	"encoding/json"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// SlipstreamState is the oracle and block fee cache of the verified dynamic
// swap hook. Fee is evaluated for a fresh transaction with zero priority fee
// and no account discount; it is not a promise for arbitrary transaction fees.
type SlipstreamState struct {
	Legacy               bool
	Factory              domain.Address
	Hook                 domain.Address
	Registry             domain.Address
	TaxModule            domain.Address
	NeedsRefresh         bool
	UnavailableReason    string
	Valid                bool
	Block                uint64
	Timestamp            uint32
	BaseFee              uint32
	SpacingFee           uint32
	FeeCap               uint32
	ScalingFactor        uint64
	DefaultFeeCap        uint32
	DefaultScalingFactor uint64
	SecondsAgo           uint32
	InitialFeeEnabled    bool
	InitialFee           uint32
	BlockFee             uint32
	ObservationIndex     uint16
	Cardinality          uint16
	CardinalityNext      uint16
	// Complete means the retained observations reach the actual oldest entry.
	// Otherwise a lookback predating the retained window is missing local data.
	Complete      bool
	Observations  OracleHistory
	TaxMultiplier uint64
	TaxMinimum    string
	TaxBaseFactor string
}

// OracleObservation retains only the tick accumulator used by the fee hook.
// Seconds-per-liquidity is not an input to this fee calculation.
type OracleObservation struct {
	Index          uint16
	Timestamp      uint32
	TickCumulative int64
}

// OracleHistory is immutable. Pool snapshots share its backing array; writes
// construct a new history, so search reads do not copy hundreds of entries.
type OracleHistory struct{ entries []OracleObservation }

func NewOracleHistory(entries []OracleObservation) OracleHistory {
	return OracleHistory{entries: slices.Clone(entries)}
}
func (h OracleHistory) Len() int                     { return len(h.entries) }
func (h OracleHistory) At(i int) OracleObservation   { return h.entries[i] }
func (h OracleHistory) MarshalJSON() ([]byte, error) { return json.Marshal(h.entries) }
func (h *OracleHistory) UnmarshalJSON(data []byte) error {
	var entries []OracleObservation
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	h.entries = entries
	return nil
}

// AppendFrom replaces the retained prefix and appends one observation in a single allocation.
func (h OracleHistory) AppendFrom(start int, next OracleObservation) OracleHistory {
	entries := make([]OracleObservation, len(h.entries)-start+1)
	copy(entries, h.entries[start:])
	entries[len(entries)-1] = next
	return OracleHistory{entries: entries}
}
