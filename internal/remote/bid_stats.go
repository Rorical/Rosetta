package remote

import (
	"bytes"
	"cmp"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// windowStats holds the closed windows' contributions to the competition
// model as decayed sums, so a decision reads sums instead of the windows. Before, every decision rebuilt each rival's presence and shares from
// all closed windows under the estimator's lock: linear in windows, 1.2 ms at
// 6,144 of them, nine decisions an offer, while the feed loop waited on the
// lock and the history was capped at 42 minutes.
//
// A window enters once when it closes, at its start time, and leaves when it
// passes the retention or a settled outcome rewrites it. A sum held as
// Σ x·2^((t−epoch)/H) is Σ x·2^(−(now−t)/H) once scaled by 2^(−(now−epoch)/H),
// the same exponential decay as before. Worth is binned at a quarter octave;
// a decision weighs each bin by worthKernel at the bin's centre. Shares are
// binned at 1/64 octave and read at the bin's upper edge, so a tip the model
// says beats a rival's bid does. Route sums are kept per pool set, the routes
// windows were opened on: a route reads the sets that share a pool with it,
// each window once, as many as there are such sets.
type windowStats struct {
	epoch     time.Time
	total     bins // all closed windows, by worth bin
	first     bins // those a rival landed first in
	routes    map[string]*routeSums
	byPool    map[domain.PoolKey][]*routeSums
	rivals    map[domain.Address]*rivalSums
	windows   int
	nextRoute uint64
}

// routeSums are the windows opened on one pool set. Its id orders the sets
// a decision reads, so their sums add up in the same order every time.
type routeSums struct {
	id           uint64
	key          string
	pools        []domain.PoolKey
	total, first bins
	rivals       map[domain.Address]*bins
	rivalWindows map[domain.Address]int
	windows      int
}

type rivalSums struct {
	windows int // contests within the retention
	any     float64
	global  bins       // by window worth bin
	shares  []shareRow // priced contests, by worth bin
	// priced and above-value contests, at bidAboveHalfLife
	priced, above float64
	lastAbove     time.Time
}

// shareRow is a rival's priced contests at one worth bin, by share bin.
type shareRow struct {
	worth  int
	shares bins
}

// bins are decayed sums in bin order, so a decision reads them in the same
// order every time. A bin whose windows have all left keeps a zero sum.
type bins []binSum

type binSum struct {
	bin int
	sum float64
}

const (
	worthBinsPerOctave = 4
	shareBinsPerOctave = 64
	unpricedBin        = math.MinInt32 // windows never priced weigh 1 at any worth
	minShareBin        = -20 * shareBinsPerOctave
	maxShareBin        = 8 * shareBinsPerOctave
	// rebaseAfter keeps the scaled sums far from float64 overflow.
	rebaseAfter = 24 * time.Hour
)

func newWindowStats(epoch time.Time) *windowStats {
	return &windowStats{epoch: epoch, routes: map[string]*routeSums{}, byPool: map[domain.PoolKey][]*routeSums{}, rivals: map[domain.Address]*rivalSums{}}
}

func worthBin(worth uint64) int {
	if worth == 0 {
		return unpricedBin
	}
	return int(math.Floor(math.Log2(float64(worth)) * worthBinsPerOctave))
}

func binWorth(bin int) float64 {
	return math.Exp2((float64(bin) + 0.5) / worthBinsPerOctave)
}

func shareBin(share float64) int {
	if share <= 0 { // a zero tip: log2 is −Inf, which int conversion leaves undefined
		return minShareBin
	}
	return min(max(int(math.Floor(math.Log2(share)*shareBinsPerOctave)), minShareBin), maxShareBin)
}

func binShare(bin int) float64 {
	return math.Exp2(float64(bin+1) / shareBinsPerOctave)
}

// growth is 2^((t−epoch)/halfLife), the weight a contribution at t carries.
func (s *windowStats) growth(t time.Time, halfLife time.Duration) float64 {
	return math.Exp2(t.Sub(s.epoch).Seconds() / halfLife.Seconds())
}

func (b *bins) add(bin int, x float64) {
	i, found := slices.BinarySearchFunc(*b, bin, func(e binSum, bin int) int { return cmp.Compare(e.bin, bin) })
	if found {
		(*b)[i].sum += x
		return
	}
	*b = slices.Insert(*b, i, binSum{bin, x})
}

// sum is Σ weight(bin)·sum in bin order.
func (b bins) sum(weight func(int) float64) float64 {
	var total float64
	for _, e := range b {
		total += weight(e.bin) * e.sum
	}
	return total
}

func (b bins) scale(f float64) {
	for i := range b {
		b[i].sum *= f
	}
}

func (r *rivalSums) addShare(worth, share int, x float64) {
	i, found := slices.BinarySearchFunc(r.shares, worth, func(e shareRow, worth int) int { return cmp.Compare(e.worth, worth) })
	if !found {
		r.shares = slices.Insert(r.shares, i, shareRow{worth: worth})
	}
	r.shares[i].shares.add(share, x)
}

// account adds a closed window's contribution with sign +1, or removes the
// contribution it added with sign −1.
func (s *windowStats) account(w *bidWindow, sign float64) {
	g := sign * s.growth(w.start, bidHalfLife)
	ga := sign * s.growth(w.start, bidAboveHalfLife)
	bin := worthBin(w.worth)
	s.windows += int(sign)
	s.total.add(bin, g)
	if w.first {
		s.first.add(bin, g)
	}
	route := s.route(w.pools)
	route.windows += int(sign)
	route.total.add(bin, g)
	if w.first {
		route.first.add(bin, g)
	}
	for _, c := range w.contest {
		r := s.rivals[c.rival()]
		if r == nil {
			r = &rivalSums{}
			s.rivals[c.rival()] = r
		}
		r.windows += int(sign)
		r.any += g
		r.global.add(bin, g)
		local := route.rivals[c.rival()]
		if local == nil {
			local = &bins{}
			route.rivals[c.rival()] = local
		}
		local.add(bin, g)
		route.rivalWindows[c.rival()] += int(sign)
		if w.worth == 0 {
			continue
		}
		share := float64(c.tip) / float64(w.worth)
		r.addShare(bin, shareBin(share), g)
		r.priced += ga
		if share > 1 {
			r.above += ga
			if sign > 0 && w.start.After(r.lastAbove) {
				r.lastAbove = w.start
			}
		}
	}
	if sign < 0 {
		s.prune(w, route)
	}
}

// route is the sums of the pool set a window was opened on.
func (s *windowStats) route(pools []domain.PoolKey) *routeSums {
	key := poolSetKey(pools)
	if r := s.routes[key]; r != nil {
		return r
	}
	s.nextRoute++
	r := &routeSums{id: s.nextRoute, key: key, rivals: map[domain.Address]*bins{}, rivalWindows: map[domain.Address]int{}}
	for i, pool := range pools {
		if slices.Contains(pools[:i], pool) {
			continue
		}
		r.pools = append(r.pools, pool)
		s.byPool[pool] = append(s.byPool[pool], r)
	}
	s.routes[key] = r
	return r
}

// poolSetKey identifies a set of pools regardless of order and repeats.
func poolSetKey(pools []domain.PoolKey) string {
	parts := make([]string, 0, len(pools))
	for _, p := range pools {
		b := make([]byte, 0, len(p.Address)+1+len(p.PoolID)+len(p.Hooks))
		b = append(b, p.Address[:]...)
		b = append(b, byte(p.Kind))
		b = append(b, p.PoolID[:]...)
		b = append(b, p.Hooks[:]...)
		parts = append(parts, string(b))
	}
	slices.Sort(parts)
	return strings.Join(slices.Compact(parts), "")
}

// prune drops rivals and routes left without windows, so the sums stay as
// large as the retention, not the history.
func (s *windowStats) prune(w *bidWindow, route *routeSums) {
	for _, c := range w.contest {
		if r := s.rivals[c.rival()]; r != nil && r.windows <= 0 {
			delete(s.rivals, c.rival())
		}
		if route.rivalWindows[c.rival()] <= 0 {
			delete(route.rivals, c.rival())
			delete(route.rivalWindows, c.rival())
		}
	}
	if route.windows > 0 {
		return
	}
	delete(s.routes, route.key)
	for _, pool := range route.pools {
		s.byPool[pool] = slices.DeleteFunc(s.byPool[pool], func(r *routeSums) bool { return r == route })
		if len(s.byPool[pool]) == 0 {
			delete(s.byPool, pool)
		}
	}
}

// rebase moves the epoch to now once it is rebaseAfter old, scaling every sum.
func (s *windowStats) rebase(now time.Time) {
	if now.Sub(s.epoch) < rebaseAfter {
		return
	}
	f, fa := 1/s.growth(now, bidHalfLife), 1/s.growth(now, bidAboveHalfLife)
	s.total.scale(f)
	s.first.scale(f)
	for _, r := range s.routes {
		r.total.scale(f)
		r.first.scale(f)
		for _, b := range r.rivals {
			b.scale(f)
		}
	}
	for _, r := range s.rivals {
		r.any *= f
		r.global.scale(f)
		for _, row := range r.shares {
			row.shares.scale(f)
		}
		r.priced *= fa
		r.above *= fa
	}
	s.epoch = now
}

// sortedRivals lists the rivals with windows in address order.
func (s *windowStats) sortedRivals() []domain.Address {
	out := make([]domain.Address, 0, len(s.rivals))
	for a := range s.rivals {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b domain.Address) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// overlapping lists the pool sets sharing a pool with route, in id order.
func (s *windowStats) overlapping(route []domain.PoolKey) []*routeSums {
	var out []*routeSums
	for _, pool := range route {
		out = append(out, s.byPool[pool]...)
	}
	slices.SortFunc(out, func(a, b *routeSums) int { return cmp.Compare(a.id, b.id) })
	return slices.Compact(out)
}
