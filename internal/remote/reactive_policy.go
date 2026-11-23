package remote

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
)

type reactivePolicy struct {
	BidTreeMode string `json:"bid_tree_mode,omitempty"` // off (default), shadow, apply
	BidTreeFile string `json:"bid_tree_file,omitempty"`

	ReplacementGainBPS uint64            `json:"replacement_min_gain_bps"`
	CancelGasLimit     uint64            `json:"cancel_gas_limit"`
	CancelFailureGas   uint64            `json:"cancel_failure_gas"`
	FundingGasLimits   map[string]uint64 `json:"funding_gas_limits,omitempty"`
	Mode               string            `json:"mode"`
	Sender             string            `json:"sender"`
	Executor           string            `json:"executor"`
	CodeHash           string            `json:"code_hash"`
	WrappedNative      string            `json:"wrapped_native"`
	Journal            string            `json:"journal"`
	Output             string            `json:"output"`
	StopFile           string            `json:"stop_file"`
	GasLimit           uint64            `json:"gas_limit"`
	RouteGasBase       uint64            `json:"route_gas_base,omitempty"`
	RouteGasPerStep    uint64            `json:"route_gas_per_step,omitempty"`
	BaseFeeHeadroomBPS uint64            `json:"base_fee_headroom_bps,omitempty"` // fee cap over base fee; default 20000
	DeadlineSeconds    uint64            `json:"deadline_seconds,omitempty"`
	BidModel           string            `json:"bid_model,omitempty"`      // shadow (default), apply or off
	FastLane           bool              `json:"fast_lane,omitempty"`      // off by default
	LongTail           bool              `json:"long_tail,omitempty"`      // send only routes that stayed profitable past the next block
	PricePrecheck      bool              `json:"price_precheck,omitempty"` // off by default; needs an executor that decodes packed v3
	SkipTaxedPools     bool              `json:"skip_taxed_pools,omitempty"`
	MetricOmm          bool              `json:"metric_omm,omitempty"`                // off by default; needs an executor with the MetricOmm leg
	CumulativePrecheck bool              `json:"price_precheck_cumulative,omitempty"` // needs price_precheck and an executor that decodes packed v4
	TipOnExpectedGas   bool              `json:"tip_on_expected_gas,omitempty"`       // needs bid_model shadow or apply for the gas estimate
	GasPriorBPS        uint64            `json:"gas_prior_bps,omitempty"`             // expected gas over the limit until receipts settle; default 6000
	BidArchive         bool              `json:"bid_archive,omitempty"`               // append windows and outcomes to <output>.bids-archive.jsonl; needs bid_model
	BidLandingMS       int64             `json:"bid_landing_ms,omitempty"`
	BidWindowMS        int64             `json:"bid_window_ms,omitempty"`
	BidMinSamples      int               `json:"bid_min_samples,omitempty"`
	MaxFee             string            `json:"max_fee_per_gas"`
	MaxTip             string            `json:"max_priority_fee_per_gas"`
	TargetTip          string            `json:"target_priority_fee_per_gas"`
	MaxTransactionFee  string            `json:"max_transaction_fee"`
	MaxDailyFee        string            `json:"max_daily_fee"`
	BidShareBPS        uint64            `json:"bid_share_bps"`
	QuoteReserveBPS    uint64            `json:"quote_reserve_bps"`
	SlippageBPS        uint64            `json:"slippage_bps"`
	LifetimeMS         int64             `json:"lifetime_ms"`
	ContextMaxAgeMS    int64             `json:"context_max_age_ms"`
	MaxCalldataBytes   int               `json:"max_calldata_bytes"`
	Assets             []struct {
		Token      string `json:"token"`
		MaxInput   string `json:"max_input"`
		MinimumNet string `json:"minimum_net_profit"`
		Wallet     bool   `json:"wallet"`
		// Inventory funds routes that start in this token from the
		// executor's own balance of it.
		Inventory bool `json:"inventory"`
	} `json:"assets"`

	// BidMinWinProbability is the least calibrated win probability a modelled
	// tip must have to be sent; default 0.2, 0 disables the gate.
	BidMinWinProbability *float64 `json:"bid_min_win_probability,omitempty"`
	// PreflightSimulation runs each intent as an eth_call against the deployed
	// executor at its quote block before it is admitted or sent; a revert drops
	// it.
	PreflightSimulation bool `json:"preflight_simulation,omitempty"`
	// UnprovenRouteCoverBPS sends a Fast intent on a route the preflight never
	// proved when its expected net is at least this share of its fee budget
	// (basis points; 10000 means the net covers the whole budget). Zero drops
	// such intents until a background probe proves the route.
	UnprovenRouteCoverBPS uint64 `json:"unproven_route_cover_bps,omitempty"`
	// HotPath sends hot routes as conditional transactions to the executor's
	// hot entry point. It needs hot_chain.
	HotPath bool `json:"hot_path,omitempty"`
	// HotChain says the executor has executeHot; hot routes then use
	// its chained form. It needs hot_path.
	HotChain bool `json:"hot_chain,omitempty"`
	// PackedConditions sends the other routes conditionally on their quiet
	// pools, keeping the packed precheck.
	PackedConditions bool `json:"packed_conditions,omitempty"`
	// WrapNative adds the wrapped native token as a 1:1 pool between native
	// currency and itself, so routes can cross a native V4 pool;
	// needs an executor with the WRAP leg.
	WrapNative bool `json:"wrap_native,omitempty"`
	// Lanes are senders beyond the primary, each with its own nonce, journal
	// and signing secret, so a send that waits for execution does not hold
	// the others back. Each must be an executor operator. The daily
	// fee budget is split evenly over all lanes.
	Lanes []struct {
		Sender   string `json:"sender"`
		SecretID string `json:"secret_id"`
		Journal  string `json:"journal"`
	} `json:"lanes,omitempty"`
	// NativeRefill keeps the sender able to pay gas: when its native balance
	// falls below Below, executor profit in wrapped native is swept to the
	// owner (the sender) and unwrapped up to Target. Wei strings.
	NativeRefill *struct {
		Below  string `json:"below"`
		Target string `json:"target"`
	} `json:"native_refill,omitempty"`
}

// defaultMinWinProbability: sends priced at win probabilities this low (tips
// orders of magnitude below the rivals') revert almost always, and each keeps
// the runtime's other routes off its pools for up to 2 s.
const defaultMinWinProbability = 0.2

type reactiveSettings struct {
	document reactivePolicy
	planning planning.ReactivePolicy
	limits   execution.DispatchLimits
	wrapped  domain.Address
	minimum  map[domain.Address]*big.Int
	wallet   map[domain.Address]bool
	// inventory marks the start assets funded from the executor's balance.
	inventory map[domain.Address]bool
	minWin    float64 // bid_min_win_probability after its default
	// refillBelow and refillTarget are native_refill; nil disables it.
	refillBelow, refillTarget *big.Int
	// lanes are the senders after the primary; their limits differ from
	// limits only in Sender.
	lanes []laneSettings
}

type laneSettings struct {
	sender   domain.Address
	secretID string
	journal  string
	limits   execution.DispatchLimits
}

func loadReactiveSettings(path string, chain domain.ChainIdentity) (reactiveSettings, error) {
	f, err := os.Open(path)
	if err != nil {
		return reactiveSettings{}, err
	}
	defer f.Close()
	var p reactivePolicy
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return reactiveSettings{}, err
	}
	if d.Decode(new(any)) != io.EOF {
		return reactiveSettings{}, fmt.Errorf("trailing reactive policy data")
	}
	if (p.Mode != "shadow" && p.Mode != "live") || p.Output == "" || p.Journal == "" || p.StopFile == "" || p.GasLimit < 21000 || p.GasLimit > 32000000 || p.LifetimeMS <= 0 || p.LifetimeMS > 2000 || p.ContextMaxAgeMS < p.LifetimeMS || p.ContextMaxAgeMS > 60000 || p.MaxCalldataBytes < 256 || p.MaxCalldataBytes > 16384 || p.BidShareBPS > 10000 || p.QuoteReserveBPS >= 10000 || p.SlippageBPS > 1000 {
		return reactiveSettings{}, fmt.Errorf("invalid reactive bounds or mode")
	}
	if filepath.Clean(p.Journal) == filepath.Clean(p.Output) || filepath.Clean(p.StopFile) == filepath.Clean(p.Journal) || filepath.Clean(p.StopFile) == filepath.Clean(p.Output) {
		return reactiveSettings{}, fmt.Errorf("reactive paths must be distinct")
	}
	if p.ReplacementGainBPS > 10000 || (p.CancelFailureGas != 0 && (p.CancelFailureGas <= 21000 || p.CancelFailureGas > p.GasLimit || p.CancelGasLimit < 21000 || p.CancelGasLimit > p.GasLimit)) {
		return reactiveSettings{}, fmt.Errorf("invalid pending-update policy")
	}
	s := reactiveSettings{document: p, minimum: map[domain.Address]*big.Int{}, wallet: map[domain.Address]bool{}, inventory: map[domain.Address]bool{}}
	c := &s.planning
	c.Chain = chain
	if c.Sender, err = domain.ParseAddress(p.Sender); err != nil || c.Sender == (domain.Address{}) {
		return s, fmt.Errorf("invalid reactive sender")
	}
	if c.Executor, err = domain.ParseAddress(p.Executor); err != nil || c.Executor == (domain.Address{}) {
		return s, fmt.Errorf("invalid reactive executor")
	}
	if c.CodeHash, err = domain.ParseHash(p.CodeHash); err != nil || c.CodeHash == (domain.Hash{}) {
		return s, fmt.Errorf("invalid executor code hash")
	}
	if s.wrapped, err = domain.ParseAddress(p.WrappedNative); err != nil || s.wrapped == (domain.Address{}) {
		return s, fmt.Errorf("invalid wrapped native")
	}
	fields := []struct {
		text string
		dst  **big.Int
		zero bool
	}{{p.MaxFee, &c.MaxFee, false}, {p.MaxTip, &c.MaxTip, true}, {p.TargetTip, &c.TargetTip, true}, {p.MaxTransactionFee, &c.MaxTotalFee, false}}
	for _, field := range fields {
		*field.dst, err = planning.Integer(field.text, field.zero)
		if err != nil {
			return s, err
		}
	}
	daily, err := planning.Integer(p.MaxDailyFee, false)
	if err != nil || daily.Cmp(c.MaxTotalFee) < 0 {
		return s, fmt.Errorf("invalid daily fee budget")
	}
	c.GasByFunding = map[execution.Funding]uint64{}
	for name, units := range p.FundingGasLimits {
		kinds := map[string]execution.Funding{"wallet": execution.FundingWallet, "v3_flash": execution.FundingV3Flash, "v3_flash_swap": execution.FundingV3FlashSwap, "v4_flash": execution.FundingV4Flash, "inventory": execution.FundingInventory}
		kind, ok := kinds[name]
		if !ok || units < 21000 || units > p.GasLimit {
			return s, fmt.Errorf("invalid funding gas limit")
		}
		c.GasByFunding[kind] = units
	}
	c.GasLimit = p.GasLimit
	if (p.RouteGasBase == 0) != (p.RouteGasPerStep == 0) || p.RouteGasPerStep > 0 && (p.RouteGasBase < 21000 || p.RouteGasBase+p.RouteGasPerStep > p.GasLimit) {
		return s, fmt.Errorf("invalid route gas limits")
	}
	c.RouteGasBase, c.RouteGasPerStep = p.RouteGasBase, p.RouteGasPerStep
	c.PricePrecheck = p.PricePrecheck
	if p.CumulativePrecheck && !p.PricePrecheck {
		return s, fmt.Errorf("price_precheck_cumulative needs price_precheck")
	}
	c.CumulativePrecheck = p.CumulativePrecheck
	c.MetricOmm = p.MetricOmm
	// Nitro drops a failed condition at the execution position; OP
	// Stack checks the parent block and its block-building check is
	// UNVERIFIED, so a hot route there could run without its pause condition.
	if (p.HotPath || p.PackedConditions) && chain.Family != domain.FamilyNitro {
		return s, fmt.Errorf("hot_path and packed_conditions need a Nitro sequencer")
	}
	if p.HotChain && !p.HotPath {
		return s, fmt.Errorf("hot_chain needs hot_path")
	}
	// The executor in contracts/src implements only the chained executeHot;
	// hot_path alone would encode executeV3Hot, which it no longer has.
	if p.HotPath && !p.HotChain {
		return s, fmt.Errorf("hot_path needs hot_chain")
	}
	c.HotPath, c.HotChain, c.PackedConditions = p.HotPath, p.HotChain, p.PackedConditions
	c.WrapNative = p.WrapNative
	c.SkipTaxedPools = p.SkipTaxedPools
	if p.BaseFeeHeadroomBPS != 0 && (p.BaseFeeHeadroomBPS < 10000 || p.BaseFeeHeadroomBPS > 40000) {
		return s, fmt.Errorf("base_fee_headroom_bps outside [10000, 40000]")
	}
	c.BaseFeeHeadroomBPS = p.BaseFeeHeadroomBPS
	// Two seconds keeps the original single-block deadline when unset.
	c.DeadlineSeconds = max(p.DeadlineSeconds, 2)
	if p.BidModel == "" {
		p.BidModel = "shadow"
	}
	if p.BidLandingMS == 0 {
		p.BidLandingMS = 400
	}
	if p.BidWindowMS == 0 {
		p.BidWindowMS = 700
	}
	if p.BidMinSamples == 0 {
		p.BidMinSamples = 50
	}
	if (p.BidModel != "shadow" && p.BidModel != "apply" && p.BidModel != "off") || p.TipOnExpectedGas && p.BidModel == "off" || p.BidArchive && p.BidModel == "off" || p.BidLandingMS < 0 || p.BidWindowMS <= p.BidLandingMS || p.BidWindowMS > 5000 || p.BidMinSamples < 1 {
		return s, fmt.Errorf("invalid reactive bid model")
	}
	if p.GasPriorBPS != 0 && (p.BidModel == "off" || p.GasPriorBPS < 1000 || p.GasPriorBPS > 10000) {
		return s, fmt.Errorf("gas_prior_bps outside [1000, 10000] or without a bid model")
	}
	if p.BidTreeMode == "" {
		p.BidTreeMode = "off"
	}
	if p.BidTreeMode != "off" && p.BidTreeMode != "shadow" && p.BidTreeMode != "apply" {
		return s, fmt.Errorf("invalid bid_tree_mode")
	}
	if p.BidTreeMode != "off" && (p.BidTreeFile == "" || p.BidModel != "apply") {
		return s, fmt.Errorf("tree models require bid_tree_file and bid_model=apply")
	}
	for _, path := range []string{p.Journal, p.Output, p.StopFile} {
		if p.BidTreeFile != "" && filepath.Clean(p.BidTreeFile) == filepath.Clean(path) {
			return s, fmt.Errorf("bid tree path must be distinct")
		}
	}
	s.minWin = defaultMinWinProbability
	if p.BidMinWinProbability != nil {
		s.minWin = *p.BidMinWinProbability
	}
	if !(s.minWin >= 0 && s.minWin < 1) {
		return s, fmt.Errorf("invalid bid_min_win_probability")
	}
	s.document = p
	if c.DeadlineSeconds > 30 {
		return s, fmt.Errorf("invalid reactive deadline")
	}
	c.BidShareBPS = p.BidShareBPS
	c.QuoteReserveBPS = p.QuoteReserveBPS
	c.SlippageBPS = p.SlippageBPS
	c.Lifetime = time.Duration(p.LifetimeMS) * time.Millisecond
	s.limits = execution.DispatchLimits{ReplacementGainBPS: p.ReplacementGainBPS, Chain: chain, Sender: c.Sender, Executor: c.Executor, CodeHash: c.CodeHash, PerTransaction: c.MaxTotalFee, PerDay: daily, StopFile: p.StopFile, Assets: map[domain.Address]*big.Int{}}
	for _, a := range p.Assets {
		token, e := domain.ParseAddress(a.Token)
		if e != nil {
			return s, e
		}
		if _, ok := s.minimum[token]; ok {
			return s, fmt.Errorf("duplicate reactive asset")
		}
		principal, e := planning.Integer(a.MaxInput, false)
		if e != nil {
			return s, e
		}
		minimum, e := planning.Integer(a.MinimumNet, false)
		if e != nil {
			return s, e
		}
		s.minimum[token] = minimum
		s.wallet[token] = a.Wallet
		if a.Inventory && token == (domain.Address{}) {
			return s, fmt.Errorf("inventory funding requires an ERC20 asset")
		}
		s.inventory[token] = a.Inventory
		s.limits.Assets[token] = principal
	}
	if len(s.minimum) == 0 {
		return s, fmt.Errorf("reactive assets required")
	}
	if err := s.loadLanes(p, daily); err != nil {
		return s, err
	}
	if r := p.NativeRefill; r != nil {
		below, e1 := planning.Integer(r.Below, false)
		target, e2 := planning.Integer(r.Target, false)
		// Below the transaction budget the context refresh already stops
		// trading, so the refill must start above it.
		if e1 != nil || e2 != nil || below.Cmp(s.limits.PerTransaction) <= 0 || target.Cmp(below) <= 0 {
			return s, fmt.Errorf("invalid native_refill: below must exceed max_transaction_fee and target must exceed below")
		}
		if _, ok := s.limits.Assets[s.wrapped]; !ok {
			return s, fmt.Errorf("native_refill requires wrapped native as a start asset")
		}
		s.refillBelow, s.refillTarget, s.limits.Wrapped = below, target, s.wrapped
	}
	return s, nil
}

// loadLanes validates the extra send lanes and splits the daily budget evenly
// over every lane, the primary included, so their sum stays within it.
func (s *reactiveSettings) loadLanes(p reactivePolicy, daily *big.Int) error {
	senders := map[domain.Address]bool{s.planning.Sender: true}
	paths := map[string]bool{filepath.Clean(p.Journal): true, filepath.Clean(p.Output): true, filepath.Clean(p.StopFile): true}
	for _, lane := range p.Lanes {
		sender, err := domain.ParseAddress(lane.Sender)
		if err != nil || sender == (domain.Address{}) || senders[sender] {
			return fmt.Errorf("invalid or repeated lane sender")
		}
		if lane.SecretID == "" || lane.Journal == "" || paths[filepath.Clean(lane.Journal)] {
			return fmt.Errorf("lane %s needs a secret and its own journal", sender)
		}
		senders[sender], paths[filepath.Clean(lane.Journal)] = true, true
		s.lanes = append(s.lanes, laneSettings{sender: sender, secretID: lane.SecretID, journal: lane.Journal})
	}
	share := new(big.Int).Quo(daily, big.NewInt(int64(len(s.lanes)+1)))
	if share.Cmp(s.limits.PerTransaction) < 0 {
		return fmt.Errorf("max_daily_fee split over %d lanes is below max_transaction_fee", len(s.lanes)+1)
	}
	s.limits.PerDay = share
	for i := range s.lanes {
		s.lanes[i].limits = s.limits
		s.lanes[i].limits.Sender = s.lanes[i].sender
	}
	return nil
}
