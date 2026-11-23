package bidreport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// Competition is feed evidence, not a causal label or canonical replay proof.
type Competition struct {
	Type       string           `json:"type"`
	Hash       domain.Hash      `json:"hash"`
	Block      uint64           `json:"block"`
	Frame      uint64           `json:"frame"`
	Index      uint64           `json:"tx_index"`
	Tip        *big.Int         `json:"tip_per_gas"`
	Pools      []domain.PoolKey `json:"pools"`
	ReceivedNS int64            `json:"received_at_ns"`
	Success    bool             `json:"success"`
	Own        bool             `json:"own"`
}

func ReadCompetition(ctx context.Context, input io.Reader, limit int) ([]Competition, error) {
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 4<<20)
	var rows []Competition
	seen := map[domain.Hash]bool{}
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var row Competition
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("competition: %w", err)
		}
		if row.Type != "competitor_tx" || !row.Success || row.Own {
			continue
		}
		if row.Hash == (domain.Hash{}) || row.ReceivedNS <= 0 || row.Tip == nil || row.Tip.Sign() < 0 {
			return nil, fmt.Errorf("invalid competition evidence")
		}
		if seen[row.Hash] {
			continue
		}
		seen[row.Hash] = true
		rows = append(rows, row)
		if len(rows) > limit {
			return nil, fmt.Errorf("competition sample limit exceeded")
		}
	}
	return rows, scan.Err()
}

type evidenceIndex map[domain.PoolKey][]Competition

func indexCompetition(rows []Competition) evidenceIndex {
	index := evidenceIndex{}
	for _, row := range rows {
		for _, key := range row.Pools {
			index[key] = append(index[key], row)
		}
	}
	for key := range index {
		slices.SortFunc(index[key], func(a, b Competition) int {
			if a.ReceivedNS < b.ReceivedNS {
				return -1
			}
			if a.ReceivedNS > b.ReceivedNS {
				return 1
			}
			return 0
		})
	}
	return index
}

// Flags can overlap. PoolMoved only proves a protection check failed; a higher
// tip predecessor does not prove that increasing our bid would change ordering.
func (index evidenceIndex) classify(p prediction, o *execution.Settlement, diagnosis string) []string {
	if o.Success || o.Cancelled {
		return nil
	}
	flags := map[string]bool{}
	// Prefer paid priority fee. A legacy signed cap is only an upper bound;
	// exceeding it still establishes a higher bid, but can miss clipped bids.
	ownTip := o.Fees.PriorityFee()
	if ownTip == nil {
		ownTip = p.intent.Call.TipCap
	}
	if strings.HasPrefix(diagnosis, "pool_moved:") {
		flags["pool_price_guard_failed"] = true
	}
	if diagnosis == "transfer_shortfall_recorded" {
		flags["execution_transfer_shortfall"] = true
	}
	if p.receivedNS > 0 {
		for _, step := range p.intent.Route.Steps {
			rows := index[step.Pool]
			begin, _ := slices.BinarySearchFunc(rows, p.receivedNS, func(row Competition, ns int64) int {
				if row.ReceivedNS < ns {
					return -1
				}
				if row.ReceivedNS > ns {
					return 1
				}
				return 0
			})
			for _, row := range rows[begin:] {
				end := p.at.UnixNano()
				if o.Position != nil {
					end = max(end, o.Position.ReceivedAtNS)
				}
				if row.ReceivedNS > end {
					break
				}
				// The trigger prefix already includes transactions through TxIndex.
				v := p.intent.Version
				if row.Block < v.BlockNumber || row.Block == v.BlockNumber && (v.Source == domain.SourceStandard || row.Index <= v.TxIndex) {
					continue
				}
				if row.ReceivedNS < p.at.UnixNano() {
					flags["shared_pool_changed_before_broadcast"] = true
				}
				pos := o.Position
				if pos != nil && row.Block == pos.BlockNumber && row.Frame == pos.FrameIndex && row.Index < pos.TransactionIndex && ownTip != nil && row.Tip.Cmp(ownTip) > 0 {
					flags["higher_tip_same_frame_predecessor"] = true
				}
			}
		}
	}
	if len(flags) == 0 {
		flags["unexplained_revert"] = true
	}
	result := make([]string, 0, len(flags))
	for flag := range flags {
		result = append(result, flag)
	}
	slices.Sort(result)
	return result
}
