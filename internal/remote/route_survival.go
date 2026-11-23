package remote

import (
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
)

// routeSurvival estimates, by route length, how often an opportunity that
// executes at its quote state still executes at the last paper delay, roughly
// when our transaction lands. Rivals take short routes within one flashblock;
// longer routes often persist, so ranking by gross alone chases the wrong
// opportunities. The paper worker writes outcomes; reactive builds read them.
// Without execution paper nothing writes outcomes and the measured prior
// alone weights the ranking.
type routeSurvival struct {
	mu       sync.Mutex
	baseline map[domain.Hash]int // group -> hops, executed at the quote state
	survived [survivalClasses]float64
	total    [survivalClasses]float64
}

const (
	survivalClasses = 7 // routes of up to 6 hops; longer ones share the last class
	survivalPrior   = 20
)

// survivalPriorRate is an OP paper measurement: 2 of 72 two- and three-hop
// opportunities and 93 of 103 four- to six-hop ones still executed after
// 600 ms. Live outcomes outweigh it after a few dozen groups.
func survivalPriorRate(hops int) float64 {
	if hops <= 3 {
		return 2.0 / 72
	}
	return 93.0 / 103
}

func newRouteSurvival() *routeSurvival {
	return &routeSurvival{baseline: map[domain.Hash]int{}}
}

func survivalClass(hops int) int { return min(max(hops, 0), survivalClasses-1) }

// observe records one paper scenario. Censored or unsupported scenarios are
// not outcomes and are not passed in.
func (s *routeSurvival) observe(group domain.Hash, hops int, final bool, delayMS int64, success bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delayMS == 0 {
		if success {
			if len(s.baseline) >= 4096 {
				clear(s.baseline) // abandoned groups; losing a few samples is harmless
			}
			s.baseline[group] = hops
		}
		return
	}
	if !final {
		return
	}
	hops, ok := s.baseline[group]
	if !ok {
		return
	}
	delete(s.baseline, group)
	class := survivalClass(hops)
	s.total[class]++
	if success {
		s.survived[class]++
	}
}

// probability is the smoothed survival rate for a route of this length. A nil
// estimator has no outcomes and returns the prior.
func (s *routeSurvival) probability(hops int) float64 {
	if s == nil {
		return survivalPriorRate(hops)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	class := survivalClass(hops)
	return (s.survived[class] + survivalPrior*survivalPriorRate(hops)) / (s.total[class] + survivalPrior)
}

// snapshot reports observed outcomes per route length for the periodic log.
func (s *routeSurvival) snapshot() map[int][2]int {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int][2]int{}
	for class := range s.total {
		if s.total[class] > 0 {
			out[class] = [2]int{int(s.survived[class]), int(s.total[class])}
		}
	}
	return out
}
