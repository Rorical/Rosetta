package remote

import (
	"math"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// precheckRevertGas is what a lost auction burns when the executor rejects a
// moved pool before swapping: 18k–31k gas measured inside the call on
// OP fork routes of 2–4 hops, plus the 21k intrinsic and calldata gas. A rival
// move spread thinly across pools still reverts at the profit check instead;
// this fixed failure-cost assumption does not capture those expensive reverts.
const precheckRevertGas = 60_000

const (
	// Closed windows are kept for bidRetention, two presence half-lives, so a
	// rival that contests one opportunity in a hundred still has a record.
	// Decisions read windowStats, not the windows, so bidWindowLimit only
	// bounds memory and the saved history: 6,144 windows can cover as little
	// as 42 minutes, which would cut the retention.
	bidRetention   = 2 * bidHalfLife
	bidWindowLimit = 65536
	bidOpenWindows = 256 // registrations still collecting rival bids
	bidDuplicate   = 200 * time.Millisecond
	// bidContinue: an opportunity readmitted on the same route within this
	// time, with no rival on it since, is the same opportunity, not another
	// one nobody contested. Re-admissions of a standing opportunity can be
	// over a third of all windows and most of the quiet ones.
	bidContinue = 5 * time.Second
	// bidSightings recent rival transactions attribute our own outcomes to
	// the rivals that shared our flashblock. An outcome waits bidOutcomeDelay
	// so that the competition writer has decoded that flashblock's rivals.
	bidSightings    = 1024
	bidOutcomeDelay = time.Second
	// A probe against a predatory rival is sent at most once per
	// bidProbeEvery per rival.
	bidProbeEvery = 5 * time.Minute
	// Gas used over gas limit, from our own receipts. Until enough have
	// settled, gasPriorRatio stands in; receipts typically use 40–45%.
	gasRatios     = 64
	gasMinRatios  = 5
	gasPriorRatio = 0.6
	// maxBid keeps predicted and observed tips below uint64 overflow.
	maxBid = math.MaxUint64 - 1
)

// bidEstimator turns rival sightings and our own outcomes into the win
// probability model of planning.ChooseTip. Each admitted opportunity opens a
// window on its route's pools. A rival on those pools within landing of the
// opportunity is provisionally labeled early (first); local receipt timing
// does not establish sequencer arrival or counterfactual ordering. A rival
// one between landing and window bid in our flashblock, and its priority fee
// is a contest. A window with neither is quiet. bid_rivals.go turns the
// windows into per-rival presence and bid-share distributions.
type bidEstimator struct {
	mu              sync.Mutex
	landing, window time.Duration
	open            []bidWindow
	windows         []bidWindow     // closed, in closing order
	sightings       []rivalSighting // a ring
	sightNext       int
	pending         []bidOutcome
	calibration     calibration
	sent            []sentOutcome // our own sends, oldest first (bid_sent.go)
	gasRatios       []float64
	// gasRatio is the ratio expectedGas applies, as float64 bits; zero
	// until gasMinRatios successes. The feed loop reads it without mu, which
	// pricing holds while it computes the model (the loop could otherwise
	// wait close to a second a minute on mu).
	gasRatio atomic.Uint64
	// gasPrior replaces gasPriorRatio for a chain whose receipts measured
	// another ratio; set once before use.
	gasPrior     float64
	routeGas     routeGasHistory
	revertRatios []float64
	revertNext   int
	gasNext      int
	probes       map[domain.Address]time.Time // last probe per predatory rival
	stats        *windowStats                 // the closed windows' sums; nil before the first

	// conditionalRatios are conditional transactions' losses, a dropped send
	// counting zero; they are persisted apart.
	conditionalRatios []float64
	conditionalNext   int

	// archive, when set, receives each window leaving retention; it must not
	// block (bidArchive.window).
	archive func(bidWindow)
}

type bidWindow struct {
	pools []domain.PoolKey
	start time.Time // identifies the window
	last  time.Time // latest admission; landing and contests count from it
	// worth is our surplus per expected gas once priced, before that the
	// opportunity's gross per expected gas at admission; 0 if unknown.
	worth   uint64
	priced  bool
	first   bool
	firstBy domain.Address
	contest []bidContest // each rival's highest tip inside our flashblock
}

type bidContest struct {
	from domain.Address
	to   domain.Address // the contract it called, zero when unknown
	tip  uint64
}

// rivalOf names the rival behind a transaction: the contract it called, or
// its sender when the contract is unknown. Operators rotate senders: one
// contract can take hundreds of transactions from dozens of senders, and by
// sender each looks like a rare rival.
func rivalOf(from, to domain.Address) domain.Address {
	if to != (domain.Address{}) {
		return to
	}
	return from
}

func (c bidContest) rival() domain.Address { return rivalOf(c.from, c.to) }

// rivalSighting is one rival transaction and where the feed showed it.
type rivalSighting struct {
	pools        []domain.PoolKey
	from, to     domain.Address
	tip          uint64
	block, frame uint64
	// single marks a transaction on one pool of ours. It may be a plain
	// swap, so it opens no contest in a window; it does say what one of our
	// sends had to beat (addSent).
	single bool
}

// bidOutcome is one of our settled sends. Raw is the model's uncalibrated
// win probability for the signed tip and worth the surplus per gas it was
// priced at; calibrate is false when the signed tip was not the model's
// (shadow mode, policy floors, the fee-tax ladder).
type bidOutcome struct {
	pools        []domain.PoolKey
	window       int64
	raw          float64
	ageNS        int64
	lane         string
	worth        uint64
	tip          uint64 // the signed tip
	calibrate    bool
	won          bool
	positioned   bool
	block, frame uint64
	at           time.Time
	// dropped marks a conditional send the sequencer voided: no receipt and
	// no position. block is then the block it was quoted in (addSent).
	dropped bool
}

func newBidEstimator(landing, window time.Duration) *bidEstimator {
	return &bidEstimator{landing: landing, window: window, probes: map[domain.Address]time.Time{}}
}

// register opens a window for an admitted opportunity worth gross wei per
// expected gas. The fast lane and the full search often admit the same route
// within one frame, and a standing opportunity is readmitted with each frame
// that touches its pools; each is one window.
func (e *bidEstimator) register(pools []domain.PoolKey, worth uint64, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expire(at)
	for i := range e.open {
		w := &e.open[i]
		if !slices.Equal(w.pools, pools) {
			continue
		}
		if at.Sub(w.start) < bidDuplicate || w.quiet() && at.Sub(w.last) <= bidContinue {
			w.last = at
			if !w.priced {
				w.worth = max(w.worth, worth)
			}
			return
		}
	}
	if len(e.open) == bidOpenWindows {
		e.open = e.open[1:]
	}
	e.open = append(e.open, bidWindow{pools: slices.Clone(pools), start: at, last: at, worth: worth})
}

func (w *bidWindow) quiet() bool { return !w.first && len(w.contest) == 0 }

// observe applies one rival transaction, received at the given feed time.
func (e *bidEstimator) observe(s rivalSighting, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expire(at)
	for i := range e.open {
		w := &e.open[i]
		elapsed := at.Sub(w.last)
		if s.single || elapsed < 0 || !overlaps(w.pools, s.pools) {
			continue
		}
		if elapsed < e.landing {
			if !w.first {
				w.first, w.firstBy = true, rivalOf(s.from, s.to)
			}
		} else if elapsed <= e.window {
			w.contend(s.from, s.to, s.tip)
		}
	}
	if len(e.sightings) < bidSightings {
		e.sightings = append(e.sightings, s)
	} else {
		e.sightings[e.sightNext] = s
		e.sightNext = (e.sightNext + 1) % bidSightings
	}
}

func (w *bidWindow) contend(from, to domain.Address, tip uint64) {
	for i := range w.contest {
		if w.contest[i].rival() == rivalOf(from, to) {
			w.contest[i].tip = max(w.contest[i].tip, tip)
			return
		}
	}
	w.contest = append(w.contest, bidContest{from: from, to: to, tip: tip})
}

// bidOutcomeOf reads one of our settled sends. Its win probability calibrates
// the model only when the signed tip is the one the model priced; decisions
// recorded before win probabilities existed have none.
func bidOutcomeOf(receipt *execution.Settlement, at time.Time) bidOutcome {
	intent, bid := receipt.Intent, receipt.Intent.Bid
	o := bidOutcome{pools: routePools(*intent), window: bid.Window, raw: bid.WinRaw, worth: bid.Worth, won: receipt.Success, at: at, ageNS: bid.AgeNS, lane: bid.Lane}
	o.calibrate = bid.Win > 0 && bid.ModelTip != nil && intent.Call.TipCap != nil && bid.ModelTip.Cmp(intent.Call.TipCap) == 0
	if intent.Call.TipCap != nil {
		o.tip = rivalTip(intent.Call.TipCap)
	}
	if p := receipt.Position; p != nil {
		o.positioned, o.block, o.frame = true, p.BlockNumber, p.FrameIndex
	}
	return o
}

// settle queues one of our outcomes; it is applied once the rivals of its
// flashblock have been seen.
func (e *bidEstimator) settle(o bidOutcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = append(e.pending, o)
	e.expire(o.at)
}

// resolve applies an outcome. Every rival that shared our flashblock on our
// pools is attributed to the window regardless of its position. Position
// alone does not prove a tip ordering; one that the timing rule took for
// landing first is a contest too, and a window we won had no first lander.
// The caller holds mu.
func (e *bidEstimator) resolve(o bidOutcome) {
	if o.calibrate {
		e.calibration.add(calibrationPoint{raw: o.raw, worth: o.worth, won: o.won, at: o.at, ageNS: o.ageNS, lane: o.lane})
	}
	e.addSent(o)
	if o.window == 0 {
		return
	}
	w, closed := e.find(o.window, o.pools)
	if w == nil {
		return
	}
	// A closed window leaves the sums while the outcome rewrites it, and its
	// contests are copied first: encode reads them outside mu.
	if closed {
		e.stats.account(w, -1)
		defer e.stats.account(w, 1)
		w.contest = slices.Clone(w.contest)
	}
	if o.won {
		w.first, w.firstBy = false, domain.Address{}
	}
	if !o.positioned {
		return
	}
	for _, s := range e.sightings {
		if s.single || s.block != o.block || s.frame != o.frame || !overlaps(s.pools, o.pools) {
			continue
		}
		if w.first && w.firstBy == rivalOf(s.from, s.to) {
			w.first, w.firstBy = false, domain.Address{}
		}
		w.contend(s.from, s.to, s.tip)
	}
}

// find is the window that opened at startMS on these pools, and whether it
// has closed. Outcomes settle seconds after their window, so the search runs
// from the newest. The caller holds mu.
func (e *bidEstimator) find(startMS int64, pools []domain.PoolKey) (*bidWindow, bool) {
	for i := range e.open {
		if w := &e.open[i]; w.start.UnixMilli() == startMS && overlaps(w.pools, pools) {
			return w, false
		}
	}
	for i := len(e.windows) - 1; i >= 0; i-- {
		if w := &e.windows[i]; w.start.UnixMilli() == startMS && overlaps(w.pools, pools) {
			return w, true
		}
	}
	return nil, false
}

// observeGas records a successful, non-cancellation receipt's gas ratio.
// The caller classifies the receipt; early reverts must never train this cost.
func (e *bidEstimator) observeGas(used, limit uint64) {
	if limit == 0 || used == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addGas(min(float64(used)/float64(limit), 1))
}

func (e *bidEstimator) addGas(ratio float64) {
	if len(e.gasRatios) < gasRatios {
		e.gasRatios = append(e.gasRatios, ratio)
	} else {
		e.gasRatios[e.gasNext] = ratio
		e.gasNext = (e.gasNext + 1) % gasRatios
	}
	if len(e.gasRatios) >= gasMinRatios {
		sorted := slices.Clone(e.gasRatios)
		slices.Sort(sorted)
		e.gasRatio.Store(math.Float64bits(sorted[len(sorted)*3/4]))
	}
}

// expectedGas is the gas a transaction with this limit is expected to use:
// the 75th percentile ratio, which errs towards paying for more gas.
func (e *bidEstimator) expectedGas(limit uint64) uint64 {
	ratio := math.Float64frombits(e.gasRatio.Load())
	if ratio == 0 {
		ratio = e.gasPrior
	}
	if ratio == 0 {
		ratio = gasPriorRatio
	}
	return max(1, uint64(math.Ceil(float64(limit)*ratio)))
}

// expire closes windows past the bid window, quiet ones only once their
// opportunity can no longer continue, and applies outcomes old enough to have
// their flashblock's rivals. The caller holds mu.
func (e *bidEstimator) expire(now time.Time) {
	kept := e.open[:0]
	for _, w := range e.open {
		limit := e.window
		if w.quiet() {
			limit = max(limit, bidContinue)
		}
		if now.Sub(w.last) <= limit {
			kept = append(kept, w)
			continue
		}
		e.addWindow(w)
	}
	clear(e.open[len(kept):])
	e.open = kept
	waiting := e.pending[:0]
	for _, o := range e.pending {
		if now.Sub(o.at) < bidOutcomeDelay {
			waiting = append(waiting, o)
			continue
		}
		e.resolve(o)
	}
	clear(e.pending[len(waiting):])
	e.pending = waiting
}

// addWindow keeps closed windows younger than bidRetention, at most
// bidWindowLimit, with their sums in stats. Windows close in about the order
// they open.
func (e *bidEstimator) addWindow(w bidWindow) {
	if e.stats == nil {
		e.stats = newWindowStats(w.start)
	}
	e.stats.rebase(w.start)
	e.windows = append(e.windows, w)
	e.stats.account(&e.windows[len(e.windows)-1], 1)
	drop := max(0, len(e.windows)-bidWindowLimit)
	for drop < len(e.windows) && w.last.Sub(e.windows[drop].last) > bidRetention {
		drop++
	}
	for i := range drop {
		e.stats.account(&e.windows[i], -1)
		if e.archive != nil {
			e.archive(e.windows[i])
		}
	}
	// Reslicing, not shifting: append copies the live windows when it next
	// grows, so dropping one costs nothing in the size of the history.
	clear(e.windows[:drop])
	e.windows = e.windows[drop:]
}

// price records the worth of the opportunity a window was opened for and
// returns the latest such window's start. A decision follows its admission
// within the landing time. The caller holds mu.
func (e *bidEstimator) price(route []domain.PoolKey, worth uint64, now time.Time) int64 {
	var start int64
	for i := range e.open {
		w := &e.open[i]
		if elapsed := now.Sub(w.last); elapsed >= 0 && elapsed < e.landing && overlaps(w.pools, route) {
			if w.priced {
				w.worth = max(w.worth, worth)
			} else {
				w.worth, w.priced = worth, true
			}
			start = max(start, w.start.UnixMilli())
		}
	}
	return start
}

// probe reports whether a probe against this predatory rival is due, and
// records it. The caller holds mu.
func (e *bidEstimator) probe(rival domain.Address, now time.Time) bool {
	if last, ok := e.probes[rival]; ok && now.Sub(last) < bidProbeEvery {
		return false
	}
	e.probes[rival] = now
	return true
}

// rivalTip keeps a bid below maxBid.
func rivalTip(tip *big.Int) uint64 {
	if tip == nil || tip.Sign() < 0 {
		return 0
	}
	if !tip.IsUint64() || tip.Uint64() > maxBid {
		return maxBid
	}
	return tip.Uint64()
}

// bestOpportunity is the one a window watches: the highest quoted gross.
func bestOpportunity(opportunities []arb.Opportunity) *arb.Opportunity {
	var best *arb.Opportunity
	for i := range opportunities {
		o := &opportunities[i]
		if o.Net != nil && (best == nil || o.Net.Cmp(best.Net) > 0) {
			best = o
		}
	}
	return best
}

// admissionWorth is an opportunity's native gross per expected gas: its size
// before costs, so a window that is never priced can still be compared with
// others. 0 when the native value is unknown.
func (s *reactiveRuntime) admissionWorth(value *big.Int, hops int) uint64 {
	if value == nil || value.Sign() <= 0 {
		return 0
	}
	cfg := s.settings.planning
	limit := cfg.GasLimit
	if cfg.RouteGasPerStep > 0 {
		limit = min(limit, cfg.RouteGasBase+cfg.RouteGasPerStep*uint64(hops))
	}
	return rivalTip(new(big.Int).Quo(value, new(big.Int).SetUint64(s.bids.expectedGas(limit))))
}

func overlaps(a, b []domain.PoolKey) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}
