package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/ethereum/go-ethereum/accounts/abi"
)

// executionPaperPolicy authorizes read-only RPC execution, never submission.
type executionPaperPolicy struct {
	Priority              *paperPriorityPolicy `json:"priority,omitempty"`
	SamplingWindowMS      int64                `json:"sampling_window_ms"`
	TipScenariosWei       []string             `json:"tip_scenarios_wei"`
	PrefixSource          string               `json:"prefix_source"`
	DelaysMS              []int64              `json:"delays_ms"`
	Artifact              string               `json:"artifact"`
	Output                string               `json:"output"`
	WrappedNative         string               `json:"wrapped_native"`
	IntervalMS            int64                `json:"interval_ms"`
	TimeoutMS             int64                `json:"timeout_ms"`
	GasLimit              uint64               `json:"gas_limit"`
	TipWei                string               `json:"tip_wei"`
	SlippageBPS           uint64               `json:"slippage_bps"`
	MaxPrefixTransactions int                  `json:"max_prefix_transactions"`
}

type paperArtifact struct {
	abi  abi.ABI
	code []byte
	hash domain.Hash
}

func loadExecutionPaper(path string) (executionPaperPolicy, paperArtifact, error) {
	p := executionPaperPolicy{SamplingWindowMS: 50, TipScenariosWei: []string{"0", "1000", "10000", "100000", "1000000"}, IntervalMS: 5000, TimeoutMS: 15000, GasLimit: 2_000_000, TipWei: "0", SlippageBPS: 50, MaxPrefixTransactions: 1000}
	var artifact paperArtifact
	raw, err := os.ReadFile(path)
	if err != nil {
		return p, artifact, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, artifact, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return p, artifact, fmt.Errorf("execution paper trailing JSON")
	}
	wrapped, err := domain.ParseAddress(p.WrappedNative)
	tip, ok := new(big.Int).SetString(p.TipWei, 10)
	if err != nil || wrapped == (domain.Address{}) || !ok || tip.Sign() < 0 || tip.BitLen() > 256 || p.Output == "" || p.Artifact == "" || p.IntervalMS < 1000 || p.IntervalMS > 3600000 || p.TimeoutMS < 100 || p.TimeoutMS > 60000 || p.GasLimit < 21000 || p.GasLimit > 1<<24 || p.SlippageBPS > 10000 || p.MaxPrefixTransactions < 1 || p.MaxPrefixTransactions > 10000 {
		return p, artifact, fmt.Errorf("invalid execution paper policy")
	}
	var priority paperPriorityGate
	if err := priority.configure(p.Priority); err != nil {
		return p, artifact, err
	}
	if p.PrefixSource != "" && p.PrefixSource != "native" && p.PrefixSource != "canonical-trace" {
		return p, artifact, fmt.Errorf("invalid paper prefix_source")
	}
	if p.SamplingWindowMS < 0 || p.SamplingWindowMS > 100 {
		return p, artifact, fmt.Errorf("sampling window must be within 0..100 ms")
	}
	if len(p.TipScenariosWei) > 16 {
		return p, artifact, fmt.Errorf("too many tip scenarios")
	}
	previous := big.NewInt(-1)
	for _, text := range p.TipScenariosWei {
		value, ok := new(big.Int).SetString(text, 10)
		if !ok || value.Sign() < 0 || value.BitLen() > 256 || value.Cmp(previous) <= 0 {
			return p, artifact, fmt.Errorf("tip scenarios must be increasing uint256 values")
		}
		previous = value
	}
	if err := validatePaperDelays(p.DelaysMS); err != nil {
		return p, artifact, err
	}
	raw, err = os.ReadFile(p.Artifact)
	if err != nil {
		return p, artifact, err
	}
	var compiled struct {
		ABI      json.RawMessage
		Bytecode struct{ Object string }
	}
	if err := json.Unmarshal(raw, &compiled); err != nil {
		return p, artifact, err
	}
	artifact.abi, err = abi.JSON(bytes.NewReader(compiled.ABI))
	if err != nil {
		return p, artifact, err
	}
	artifact.code, err = hexData(compiled.Bytecode.Object)
	if err != nil || len(artifact.code) == 0 || len(artifact.code) > 49120 {
		return p, artifact, fmt.Errorf("invalid executor creation bytecode")
	}
	for _, method := range []string{"setPaused", "configVersion", "executePacked", "owner"} {
		if _, ok := artifact.abi.Methods[method]; !ok {
			return p, artifact, fmt.Errorf("executor artifact lacks %s", method)
		}
	}
	artifact.hash = digest(artifact.code)
	return p, artifact, nil
}

func validatePaperDelays(delays []int64) error {
	if len(delays) > 4 {
		return fmt.Errorf("execution paper allows at most four delays")
	}
	previous := int64(0)
	for _, delay := range delays {
		if delay <= previous || delay > 2000 {
			return fmt.Errorf("execution paper delays must be increasing within 1..2000 ms")
		}
		previous = delay
	}
	return nil
}
