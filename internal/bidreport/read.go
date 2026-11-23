package bidreport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

type prediction struct {
	intent     *execution.ReactiveIntent
	at         time.Time
	lane       string
	receivedNS int64
}

type record struct {
	Diagnosis   string                    `json:"diagnosis"`
	Type        string                    `json:"type"`
	Mode        string                    `json:"mode"`
	Transaction domain.Hash               `json:"transaction"`
	Intent      *execution.ReactiveIntent `json:"intent"`
	Receipt     *execution.Settlement     `json:"receipt"`
	Submission  struct {
		StartedAtNS int64
	} `json:"submission"`
	Timing struct {
		Lane       string `json:"lane"`
		ReceivedNS int64  `json:"received_ns"`
	} `json:"timing"`
}

// Read joins pre-submission predictions to final canonical outcomes by hash.
// Feed bookings are provisional. Later canonical corrections replace earlier
// results; voided variants, unknown results and changed tips are not losses.
func Read(ctx context.Context, input io.Reader, opts Options) (Report, error) {
	if opts.Limit < 1 || opts.Limit > 200000 || !opts.Until.IsZero() && !opts.Since.Before(opts.Until) {
		return Report{}, fmt.Errorf("invalid limit or time interval")
	}
	predictions := map[domain.Hash]prediction{}
	outcomes := map[domain.Hash]*execution.Settlement{}
	voided := map[domain.Hash]bool{}
	diagnoses := map[domain.Hash]string{}
	var chain domain.ChainIdentity
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 65536), 4<<20)
	line := 0
	for scan.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		var row record
		if err := json.Unmarshal(scan.Bytes(), &row); err != nil {
			return Report{}, fmt.Errorf("bid journal line %d: %w", line, err)
		}
		switch row.Type {
		case "reactive_intent":
			if row.Mode != "live" || row.Transaction == (domain.Hash{}) {
				continue
			}
			if row.Intent == nil || row.Submission.StartedAtNS <= 0 {
				return Report{}, fmt.Errorf("bid journal line %d: submitted prediction lacks intent/time", line)
			}
			at := time.Unix(0, row.Submission.StartedAtNS).UTC()
			if !opts.Since.IsZero() && at.Before(opts.Since) || !opts.Until.IsZero() && !at.Before(opts.Until) {
				continue
			}
			if row.Intent.Chain.ChainID == 0 {
				return Report{}, fmt.Errorf("bid journal line %d: missing chain identity", line)
			}
			if chain.ChainID != 0 && chain != row.Intent.Chain {
				return Report{}, fmt.Errorf("bid journal line %d: mixed chains", line)
			}
			chain = row.Intent.Chain
			lane := row.Timing.Lane
			if lane == "" {
				lane = "full"
			}
			if prior, ok := predictions[row.Transaction]; ok {
				if prior.intent.ID != row.Intent.ID {
					return Report{}, fmt.Errorf("bid journal line %d: conflicting prediction for a hash", line)
				}
				continue // do not move the original send time on a rebroadcast
			}
			predictions[row.Transaction] = prediction{row.Intent, at, lane, row.Timing.ReceivedNS}
		case "settlement":
			r := row.Receipt
			if r == nil || r.Hash == (domain.Hash{}) {
				return Report{}, fmt.Errorf("bid journal line %d: missing settlement identity", line)
			}
			if !strings.HasPrefix(r.Basis, "canonical_") {
				continue
			}
			outcomes[r.Hash] = r
			delete(voided, r.Hash)
		case "revert_diagnosis":
			diagnoses[row.Transaction] = row.Diagnosis
		case "settlement_voided":
			delete(outcomes, row.Transaction)
			voided[row.Transaction] = true
		}
		if len(predictions)+len(outcomes)+len(voided)+len(diagnoses) > opts.Limit {
			return Report{}, fmt.Errorf("bid journal sample limit exceeded")
		}
	}
	if err := scan.Err(); err != nil {
		return Report{}, fmt.Errorf("read bid journal: %w", err)
	}
	return summarize(predictions, outcomes, voided, diagnoses, indexCompetition(opts.Competition))
}
