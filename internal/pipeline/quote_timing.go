package pipeline

import (
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
)

// QuoteTiming separates computation from publication/research queue latency.
// BasisVersion remains the original prefix when an unchanged quote is carried.
type QuoteTiming struct {
	Stages           arb.SearchStages    `json:"stages_ns"`
	Stage            arb.QuoteStage      `json:"stage,omitempty"`
	ReceivedAtNS     int64               `json:"received_at_ns,omitempty"`
	QuotedAtNS       int64               `json:"quoted_at_ns,omitempty"`
	RecordedAtNS     int64               `json:"recorded_at_ns"`
	ReceiveToQuoteUS int64               `json:"receive_to_quote_us,omitempty"`
	BasisVersion     domain.StateVersion `json:"basis_version"`
}

func quoteTiming(event arb.SearchEvent, at time.Time) QuoteTiming {
	out := QuoteTiming{Stages: event.Stages, Stage: event.Stage, RecordedAtNS: at.UnixNano(), BasisVersion: event.OriginalJob().Version}
	if !event.Job.ReceivedAt.IsZero() {
		out.ReceivedAtNS = event.Job.ReceivedAt.UnixNano()
	}
	if !event.QuotedAt.IsZero() {
		out.QuotedAtNS = event.QuotedAt.UnixNano()
		if out.ReceivedAtNS != 0 {
			out.ReceiveToQuoteUS = max(0, event.QuotedAt.Sub(event.Job.ReceivedAt).Microseconds())
		}
	}
	return out
}
