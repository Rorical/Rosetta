package arb

import (
	"slices"
	"testing"
	"time"
)

// Phase metrics of BenchmarkBurstBlock: how long the short phase held the
// lane, what became of the long routes, and how many frames a search absorbed.
func init() {
	burstExtra = func(b *testing.B, observations []SearchObservation, blocks float64) {
		var short, long []time.Duration
		outcomes := map[LongOutcome]int{}
		frames, completed, resumed := 0, 0, 0
		for _, o := range observations {
			if o.Outcome != SearchCompleted {
				continue
			}
			completed++
			frames += o.Frames
			outcomes[o.Long]++
			short = append(short, o.Stages.Short)
			if o.Long == LongCompleted {
				long = append(long, o.Stages.Long)
			}
			if o.Resumed {
				resumed++
			}
		}
		quantile := func(values []time.Duration, q float64) float64 {
			if len(values) == 0 {
				return 0
			}
			slices.Sort(values)
			return float64(values[int(q*float64(len(values)-1))].Microseconds()) / 1000
		}
		b.ReportMetric(float64(outcomes[LongCompleted])/blocks, "long-completed/block")
		b.ReportMetric(float64(outcomes[LongDeferred])/blocks, "long-deferred/block")
		b.ReportMetric(float64(outcomes[LongPreempted])/blocks, "long-preempted/block")
		b.ReportMetric(float64(resumed)/blocks, "resumed/block")
		b.ReportMetric(quantile(short, 0.5), "short-phase-p50-ms")
		b.ReportMetric(quantile(short, 0.9), "short-phase-p90-ms")
		b.ReportMetric(quantile(long, 0.5), "long-phase-p50-ms")
		if completed > 0 {
			b.ReportMetric(float64(frames)/float64(completed), "frames/search")
		}
	}
}
