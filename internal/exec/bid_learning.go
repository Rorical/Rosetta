package exec

// BidLearning binds training inputs and shadow predictions to the exact sealed
// action. Unsent/replaced actions never acquire invented outcome labels.
type BidLearning struct {
	Schema            string    `json:"schema"`
	Features          []float64 `json:"features"`
	ModelID           string    `json:"model_id,omitempty"`
	Mode              string    `json:"mode"`
	Status            string    `json:"status"`
	Applied           bool      `json:"applied,omitempty"`
	BaselineWin       float64   `json:"baseline_win"`
	BaselineRevertGas uint64    `json:"baseline_revert_gas"`
	TreeWin           float64   `json:"tree_win,omitempty"`
	TreeRevertGas     uint64    `json:"tree_revert_gas,omitempty"`
	RemainingMS       float64   `json:"remaining_ms,omitempty"`
}
