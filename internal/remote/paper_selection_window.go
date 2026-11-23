package remote

import "time"

// Retain only the currently selected immutable candidate snapshot. Refinement
// events can replace it before admission; an executing group is never replaced.
// This paper-only wait does not hold the live search or sequencer path.
type paperSelectionWindow struct {
	job   paperExecutionJob
	until time.Time
}

func (p *executionPaper) flushSelection(now time.Time, force bool) {
	if p.window == nil || !force && now.Before(p.window.until) {
		return
	}
	job := p.window.job
	p.window = nil
	job.selection.WindowWaitNS = now.Sub(job.discovered).Nanoseconds()
	p.sampler.admit(job.selection, job.opportunity)
	p.schedule(job)
}
