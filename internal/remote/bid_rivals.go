package remote

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
)

// The win probability model (planning.WinModel) is built for each decision
// from the closed windows' sums (bid_stats.go). It rests on two observations:
//
//   - Rivals bid in proportion to the opportunity when they contest it. A
//     rival whose median tip is a few wei per gas can bid tens of thousands
//     of times that on valuable opportunities, and winners typically take
//     a quarter to a third of the opportunity's worth. A bid is therefore
//     read as a share of the window's worth (our surplus per expected gas).
//   - The rival that bids most often is not the one that wins: a winner may
//     rarely have been seen on the route. Every rival that may show up
//     counts, each with its own presence, plus one not yet seen.
//
// Presence π_i on the route is rival i's decayed share of the route's windows,
// shrunk toward its share of all windows, which is shrunk toward a prior for
// an arbitrary rival. Windows count by the same worth kernel as bid shares:
// a rival can win nearly half the frames we lose yet show about 1% presence,
// because it contests valuable opportunities and the many small ones would
// count as its absences. Near a worth seen rarely, the kernel mass is small, so that
// rate is shrunk toward the rival's rate at any worth with bidWorthStrength,
// and only that one toward the prior. Its bid share comes from its own priced windows,
// weighted by how close their worth is to this one, shrunk toward the global
// contested-share distribution, itself shrunk toward a fixed prior. A rival
// whose own history is thin, or lies at a very different worth, therefore
// bids like contested rivals in general bid at this worth.
//
// Some observed bids exceed our estimated worth (share > 1); the rival's
// own value and motive are unknown. They count in its distribution and the
// global one: no tip within our ceiling beats them, so they lower the win
// probability without raising our bids, and no single rival may carry more
// than bidGlobalRivalMass of the global one.
const (
	bidHalfLife = time.Hour // presence, shares and calibration
	// bidWorthBand is the worth ratio at which a window's weight in a share
	// distribution falls to e^-1/2; at 16 times it is 0.14.
	bidWorthBand = 4.0
	// Presence: a rival's global rate is shrunk toward bidPresencePrior with
	// bidGlobalStrength windows, and its route rate toward the global rate with
	// bidRouteStrength windows.
	bidPresencePrior  = 0.02
	bidGlobalStrength = 20.0
	bidRouteStrength  = 10.0
	bidWorthStrength  = 5.0
	// First landers, the same way.
	bidFirstPrior = 0.1
	// A rival not yet seen: the Good–Turing rate of windows whose contest
	// includes a rival seen only once, shrunk toward bidUnknownPrior.
	bidUnknownPrior = 0.05
	// Shares: a rival's own distribution against the global with
	// bidOwnStrength windows of weight; the global empirical one against
	// bidSharePrior with bidPriorStrength.
	bidOwnStrength     = 3.0
	bidPriorStrength   = 4.0
	bidGlobalRivalMass = 6.0 // most weight one rival carries in the global shares
	bidOwnAtoms        = 16
	bidGlobalAtoms     = 32
	bidContenders      = 24 // rivals modelled individually, by presence
	// Above-value bidding: the share of a rival's recent priced contests bid
	// above the worth (bidAboveStrength windows of prior keep one such bid from
	// marking it), halved every bidAboveHalfLife since its last one, since
	// the historical above-value label should not persist indefinitely. A rival whose rate reaches
	// bidPredatorRate is predatory.
	bidAboveHalfLife = 30 * time.Minute
	bidAboveStrength = 1.0
	bidPredatorRate  = 0.25
)

// bidSharePrior is what a contested opportunity draws when nothing similar
// has been seen: winning rivals typically bid a quarter to a third of the
// worth, and a single rival's shares on valuable windows can span 1–52%.
// Each point carries equal weight.
var bidSharePrior = []float64{0.05, 0.1, 0.2, 0.3, 0.35, 0.4, 0.5, 0.6}

type shareAtom struct {
	share, weight float64
}

func byShare(a, b shareAtom) int { return cmp.Compare(a.share, b.share) }

// rivalStats is one rival's evidence over the closed windows.
type rivalStats struct {
	address        domain.Address
	any            float64     // decayed windows contested at any worth
	global, local  float64     // the same at this worth, all and on the route
	own            []shareAtom // ascending
	ownMass        float64
	priced, above  float64 // above-value half-life weights of priced contests
	lastAbove      time.Time
	presence, rate float64
}

// bidModel is the decision's view of the competition.
type bidModel struct {
	win                   planning.WinModel
	unknown               float64
	windows, routeWindows int
	rivals                []*rivalStats // in contender order
}

func decay(age, halfLife time.Duration) float64 {
	return math.Exp2(-max(age, 0).Seconds() / halfLife.Seconds())
}

// worthKernel weighs a window priced at worth against the opportunity at hand.
func worthKernel(worth, target uint64) float64 {
	if target == 0 {
		return 1
	}
	x := math.Log(float64(worth)/float64(target)) / math.Log(bidWorthBand)
	return math.Exp(-x * x / 2)
}

// model builds the win probability model for an opportunity of the given
// worth on route from the closed windows' sums (windowStats). Its cost is in
// rivals and bins, not windows. The caller holds mu.
func (e *bidEstimator) model(now time.Time, route []domain.PoolKey, worth uint64) bidModel {
	s := e.stats
	if s == nil {
		s = newWindowStats(now)
	}
	scale, scaleAbove := 1/s.growth(now, bidHalfLife), 1/s.growth(now, bidAboveHalfLife)
	// Worth bins cover 0..64·worthBinsPerOctave; NaN marks one not yet weighed.
	kernels := make([]float64, 64*worthBinsPerOctave+1)
	for i := range kernels {
		kernels[i] = math.NaN()
	}
	kernel := func(bin int) float64 {
		if bin == unpricedBin {
			return 1
		}
		if math.IsNaN(kernels[bin]) {
			kernels[bin] = worthKernel(uint64(binWorth(bin)), worth)
		}
		return kernels[bin]
	}
	one := func(int) float64 { return 1 }
	routes := s.overlapping(route)
	anyTotal, anyFirst := scale*s.total.sum(one), scale*s.first.sum(one)
	total, first := scale*s.total.sum(kernel), scale*s.first.sum(kernel)
	var local, firstLocal float64
	routeWindows := 0
	locals := map[domain.Address]float64{}
	for _, r := range routes {
		local += scale * r.total.sum(kernel)
		firstLocal += scale * r.first.sum(kernel)
		routeWindows += r.windows
		for address, b := range r.rivals {
			locals[address] += scale * b.sum(kernel)
		}
	}
	// Good–Turing: how often a window brings a rival seen only once. A rival
	// with one contest holds just that window in its sums.
	var singletons float64
	var rivals []*rivalStats
	sums := make([]float64, maxShareBin-minShareBin+1)
	weight := func(bin int) float64 { return scale * kernel(bin) }
	for _, address := range s.sortedRivals() {
		r := s.rivals[address]
		st := &rivalStats{address: address, any: scale * r.any, global: scale * r.global.sum(kernel), local: locals[address]}
		if r.windows == 1 {
			singletons += st.global
		}
		st.own, st.ownMass = ownShares(r, weight, sums)
		st.priced, st.above, st.lastAbove = scaleAbove*r.priced, scaleAbove*r.above, r.lastAbove
		rivals = append(rivals, st)
	}

	global := globalShares(rivals)
	firstAny := (anyFirst + bidGlobalStrength*bidFirstPrior) / (anyTotal + bidGlobalStrength)
	firstGlobal := (first + bidWorthStrength*firstAny) / (total + bidWorthStrength)
	m := bidModel{windows: s.windows, routeWindows: routeWindows}
	m.win.First = (firstLocal + bidRouteStrength*firstGlobal) / (local + bidRouteStrength)
	m.win.Shift = e.calibration.shift(worth)
	m.unknown = (singletons + bidGlobalStrength*bidUnknownPrior) / (total + bidGlobalStrength)
	for _, st := range rivals {
		anyRate := (st.any + bidGlobalStrength*bidPresencePrior) / (anyTotal + bidGlobalStrength)
		rate := (st.global + bidWorthStrength*anyRate) / (total + bidWorthStrength)
		st.presence = (st.local + bidRouteStrength*rate) / (local + bidRouteStrength)
		if st.above > 0 {
			st.rate = st.above / (st.priced + bidAboveStrength) * decay(now.Sub(st.lastAbove), bidAboveHalfLife)
		}
	}
	slices.SortStableFunc(rivals, func(a, b *rivalStats) int {
		switch {
		case a.presence > b.presence:
			return -1
		case a.presence < b.presence:
			return 1
		}
		return 0
	})
	m.rivals = rivals[:min(len(rivals), bidContenders)]
	for _, st := range m.rivals {
		own := st.ownMass / (st.ownMass + bidOwnStrength)
		shares := mix(compress(st.own, bidOwnAtoms), own, global, 1-own)
		m.win.Contenders = append(m.win.Contenders, planning.Contender{Name: st.address.Hex(), Presence: st.presence, Bids: tips(shares, worth), Predatory: st.rate >= bidPredatorRate})
	}
	m.win.Contenders = append(m.win.Contenders, planning.Contender{Name: "unknown", Presence: m.unknown, Bids: tips(global, worth)})
	return m
}

// globalShares is the contested-share distribution of all rivals at this
// worth: at most bidGlobalRivalMass of weight per rival, shrunk toward
// bidSharePrior.
func globalShares(rivals []*rivalStats) []shareAtom {
	lists := make([][]shareAtom, 0, len(rivals))
	var mass float64
	for _, st := range rivals {
		scale := 1.0
		if st.ownMass > bidGlobalRivalMass {
			scale = bidGlobalRivalMass / st.ownMass
		}
		atoms := make([]shareAtom, len(st.own))
		for i, a := range st.own {
			atoms[i] = shareAtom{a.share, a.weight * scale}
		}
		lists = append(lists, atoms)
		mass += st.ownMass * scale
	}
	atoms := mergeShares(lists)
	prior := make([]shareAtom, len(bidSharePrior))
	for i, share := range bidSharePrior {
		prior[i] = shareAtom{share, 1}
	}
	empirical := mass / (mass + bidPriorStrength)
	return mix(compress(atoms, bidGlobalAtoms), empirical, prior, 1-empirical)
}

// ownShares is a rival's priced contests at this worth as ascending shares,
// one atom per share bin, and their total weight. weight is a worth bin's
// decayed kernel. sums, indexed from minShareBin, must be zero; it is left
// zero for the next rival.
func ownShares(r *rivalSums, weight func(int) float64, sums []float64) ([]shareAtom, float64) {
	var touched []int
	for _, row := range r.shares {
		k := weight(row.worth)
		for _, e := range row.shares {
			// Windows hundreds of times off this worth, or many hours old,
			// add nothing to a share distribution but its cost.
			if w := k * e.sum; w >= 1e-4 {
				i := e.bin - minShareBin
				if sums[i] == 0 {
					touched = append(touched, i)
				}
				sums[i] += w
			}
		}
	}
	slices.Sort(touched)
	own := make([]shareAtom, len(touched))
	var mass float64
	for j, i := range touched {
		own[j] = shareAtom{binShare(i + minShareBin), sums[i]}
		mass += sums[i]
		sums[i] = 0
	}
	return own, mass
}

// mergeShares merges ascending share lists into one, adding the weights of
// equal shares.
func mergeShares(lists [][]shareAtom) []shareAtom {
	for len(lists) > 1 {
		next := lists[:0:0]
		for i := 0; i < len(lists); i += 2 {
			if i+1 == len(lists) {
				next = append(next, lists[i])
				break
			}
			next = append(next, mergeTwo(lists[i], lists[i+1]))
		}
		lists = next
	}
	if len(lists) == 0 {
		return nil
	}
	return lists[0]
}

func mergeTwo(a, b []shareAtom) []shareAtom {
	out := make([]shareAtom, 0, len(a)+len(b))
	for len(a) > 0 && len(b) > 0 {
		switch {
		case a[0].share < b[0].share:
			out, a = append(out, a[0]), a[1:]
		case a[0].share > b[0].share:
			out, b = append(out, b[0]), b[1:]
		default:
			out = append(out, shareAtom{a[0].share, a[0].weight + b[0].weight})
			a, b = a[1:], b[1:]
		}
	}
	out = append(out, a...)
	return append(out, b...)
}

// compress reduces weighted shares to at most k atoms of equal weight at
// evenly spaced weighted quantiles, sorted ascending. Atoms of equal share
// yield the same quantile in any order.
func compress(atoms []shareAtom, k int) []shareAtom {
	sorted := atoms
	if !slices.IsSortedFunc(atoms, byShare) {
		sorted = slices.Clone(atoms)
		slices.SortFunc(sorted, byShare)
	}
	if len(sorted) <= k {
		return sorted
	}
	var total float64
	for _, a := range sorted {
		total += a.weight
	}
	out := make([]shareAtom, k)
	cumulative, i := 0.0, 0
	for j := range out {
		target := total * (2*float64(j) + 1) / (2 * float64(k))
		for i < len(sorted)-1 && cumulative+sorted[i].weight < target {
			cumulative += sorted[i].weight
			i++
		}
		out[j] = shareAtom{sorted[i].share, total / float64(k)}
	}
	return out
}

// mix is the mixture of two share distributions with the given weights;
// each is normalized first, and an empty one contributes nothing.
func mix(a []shareAtom, wa float64, b []shareAtom, wb float64) []shareAtom {
	var out []shareAtom
	norm := func(atoms []shareAtom, weight float64) {
		var total float64
		for _, x := range atoms {
			total += x.weight
		}
		if total <= 0 || weight <= 0 {
			return
		}
		for _, x := range atoms {
			out = append(out, shareAtom{x.share, x.weight / total * weight})
		}
	}
	norm(a, wa)
	norm(b, wb)
	var total float64
	for _, x := range out {
		total += x.weight
	}
	for i := range out {
		out[i].weight /= total
	}
	return out
}

// tips turns shares of worth into bids per gas, rounded up.
func tips(shares []shareAtom, worth uint64) []planning.BidAtom {
	bids := make([]planning.BidAtom, len(shares))
	for i, s := range shares {
		bids[i] = planning.BidAtom{Tip: scaleBid(worth, s.share), Weight: s.weight}
	}
	return bids
}

// top lists the n rivals that cost the most win probability at tip.
func (m bidModel) top(tip uint64, n int) []execution.BidRival {
	type entry struct {
		rival execution.BidRival
		loss  float64
	}
	var entries []entry
	for i, st := range m.rivals {
		c := m.win.Contenders[i]
		var loss float64
		for _, b := range c.Bids {
			if b.Tip >= tip {
				loss += b.Weight
			}
		}
		own := st.ownMass / (st.ownMass + bidOwnStrength)
		entries = append(entries, entry{execution.BidRival{Address: c.Name, Presence: round4(c.Presence), Bid: median(c.Bids), Own: round4(own), AboveValue: round4(st.rate)}, c.Presence * loss})
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		switch {
		case a.loss > b.loss:
			return -1
		case a.loss < b.loss:
			return 1
		}
		return 0
	})
	out := make([]execution.BidRival, 0, n)
	for _, e := range entries[:min(n, len(entries))] {
		out = append(out, e.rival)
	}
	return out
}

func median(bids []planning.BidAtom) uint64 {
	sorted := slices.Clone(bids)
	slices.SortStableFunc(sorted, func(a, b planning.BidAtom) int {
		switch {
		case a.Tip < b.Tip:
			return -1
		case a.Tip > b.Tip:
			return 1
		}
		return 0
	})
	var cumulative float64
	for _, b := range sorted {
		cumulative += b.Weight
		if cumulative >= 0.5 {
			return b.Tip
		}
	}
	if len(sorted) == 0 {
		return 0
	}
	return sorted[len(sorted)-1].Tip
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

// scaleBid is a predicted bid, rounded up and kept below maxBid.
func scaleBid(bid uint64, ratio float64) uint64 {
	x := math.Ceil(float64(bid) * ratio)
	switch {
	case x < 1:
		return 1
	case x >= 0x1p64:
		return maxBid
	}
	return min(uint64(x), maxBid)
}
