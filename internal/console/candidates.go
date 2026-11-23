package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

const kindCandidate = "candidate"

// candidateLine is one -candidate-out record. Opportunity.Net is the search's
// own gross: output minus input in the start asset, before any network cost.
// The optional economics block is research's estimate, whose network fee is
// calibrated on other senders' observed transaction costs; it is kept only as
// a labeled pessimistic figure, never as the candidate's net.
type candidateLine struct {
	Economics *struct {
		Basis      string   `json:"basis"`
		NetworkFee *big.Int `json:"network_fee"`
		Net        *big.Int `json:"estimated_net"`
	} `json:"economics"`
	Timing struct {
		RecordedAtNS int64 `json:"recorded_at_ns"`
	} `json:"timing"`
	Version     domain.StateVersion `json:"version"`
	Opportunity struct {
		Cycle struct {
			Pools []domain.PoolKey
			Start domain.Address
		}
		Input *big.Int
		Net   *big.Int
	} `json:"opportunity"`
}

type candidateRow struct {
	Source, Asset, Input, Gross             string
	Block                                   uint64
	RecordedMS                              int64
	GrossValue                              float64
	Route                                   []poolRef
	ResearchNet, ResearchFee, ResearchBasis string
}

func parseCandidate(line []byte, into *batch) error {
	var candidate candidateLine
	if err := json.Unmarshal(line, &candidate); err != nil {
		return fmt.Errorf("decode candidate: %w", err)
	}
	opportunity := candidate.Opportunity
	if opportunity.Input == nil || opportunity.Net == nil {
		return fmt.Errorf("candidate lacks input or gross")
	}
	row := candidateRow{Source: sourceName(candidate.Version.Source), Block: candidate.Version.BlockNumber, RecordedMS: candidate.Timing.RecordedAtNS / 1e6,
		Asset: opportunity.Cycle.Start.Hex(), Input: opportunity.Input.String(), Gross: opportunity.Net.String(), GrossValue: approximate(opportunity.Net),
		Route: poolRefs(opportunity.Cycle.Pools)}
	if economics := candidate.Economics; economics != nil && economics.Net != nil {
		row.ResearchNet, row.ResearchFee, row.ResearchBasis = economics.Net.String(), decimal(economics.NetworkFee), economics.Basis
	}
	into.candidates = append(into.candidates, row)
	return nil
}

func writeCandidates(ctx context.Context, tx *sql.Tx, instance string, rows []candidateRow) error {
	for _, row := range rows {
		route, err := json.Marshal(row.Route)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO candidates(instance, source, block, recorded_ms, asset, input, gross, gross_value, hops, route, research_net, research_fee, research_basis)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, instance, row.Source, row.Block, row.RecordedMS, row.Asset, row.Input, row.Gross, row.GrossValue, len(row.Route), string(route),
			row.ResearchNet, row.ResearchFee, row.ResearchBasis); err != nil {
			return err
		}
	}
	return nil
}

// ResearchEstimate is research's pessimistic after-cost figure for a candidate.
type ResearchEstimate struct {
	Net        string `json:"net"`
	NetworkFee string `json:"network_fee"`
	Basis      string `json:"basis"`
}

// Candidate is one search result. GrossNative values the gross in wei at the
// admission rate of our newest send of the same asset, when one exists.
type Candidate struct {
	ID          int64             `json:"id"`
	RecordedMS  int64             `json:"recorded_ms"`
	Source      string            `json:"source"`
	Block       uint64            `json:"block"`
	Asset       string            `json:"asset"`
	Input       string            `json:"input"`
	Gross       string            `json:"gross"`
	GrossNative *float64          `json:"gross_native,omitempty"`
	Hops        int               `json:"hops"`
	Route       []poolRef         `json:"route"`
	Research    *ResearchEstimate `json:"research,omitempty"`
}

var candidateOrders = map[string]string{"": "recorded_ms DESC, id DESC", "recent": "recorded_ms DESC, id DESC", "gross": "gross_value DESC, id DESC"}

func (s *Store) Candidates(ctx context.Context, instance, source, sort string, limit int) ([]Candidate, error) {
	order, ok := candidateOrders[sort]
	if !ok || (source != "" && source != "fast" && source != "standard") {
		return nil, fmt.Errorf("%w: candidate source or sort", errBadQuery)
	}
	rates, err := s.nativeRates(ctx, instance)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, recorded_ms, source, block, asset, input, gross, hops, route, research_net, research_fee, research_basis
		FROM candidates WHERE instance=? AND (?='' OR source=?) ORDER BY `+order+` LIMIT ?`, instance, source, source, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []Candidate{}
	for rows.Next() {
		var c Candidate
		var route, researchNet, researchFee, researchBasis string
		if err := rows.Scan(&c.ID, &c.RecordedMS, &c.Source, &c.Block, &c.Asset, &c.Input, &c.Gross, &c.Hops, &route, &researchNet, &researchFee, &researchBasis); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(route), &c.Route); err != nil {
			return nil, fmt.Errorf("decode stored route: %w", err)
		}
		if researchNet != "" {
			c.Research = &ResearchEstimate{Net: researchNet, NetworkFee: researchFee, Basis: researchBasis}
		}
		if rate, ok := rates[c.Asset]; ok {
			if gross, ok := new(big.Int).SetString(c.Gross, 10); ok {
				value := approximate(toNative(gross, rate[0], rate[1]))
				c.GrossNative = &value
			}
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

// nativeRates is each asset's conversion from our newest send of it. The zero
// address is the native token itself.
func (s *Store) nativeRates(ctx context.Context, instance string) (map[string][2]*big.Int, error) {
	rates := map[string][2]*big.Int{(domain.Address{}).Hex(): {big.NewInt(1), big.NewInt(1)}}
	rows, err := s.db.QueryContext(ctx, `SELECT asset, conv_num, conv_den, MAX(sent_ms) FROM sends WHERE instance=? AND conv_num<>'' AND conv_den<>'' GROUP BY asset`, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var asset, numerator, denominator string
		var newest int64
		if err := rows.Scan(&asset, &numerator, &denominator, &newest); err != nil {
			return nil, err
		}
		num, okNum := new(big.Int).SetString(numerator, 10)
		den, okDen := new(big.Int).SetString(denominator, 10)
		if okNum && okDen && num.Sign() > 0 && den.Sign() > 0 {
			rates[asset] = [2]*big.Int{num, den}
		}
	}
	return rates, rows.Err()
}
