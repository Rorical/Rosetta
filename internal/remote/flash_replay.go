package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/params"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// openFlashSnapshot replays signed transactions through the node's native OP
// engine. All later reads/traces use the fixed parent plus private overrides.
func openFlashSnapshot(ctx context.Context, client *rpc.Client, anchor rpc.ExecutionBlock, chain domain.ChainIdentity, deployments domain.Deployments, prefix flashPrefix) (*snapshot, *execution.FlashblocksEvidence, error) {
	if chain.Family != domain.FamilyOPStack || prefix.Version.Anchor != anchor.Ref || prefix.Version.Source != domain.SourceFast {
		return nil, nil, fmt.Errorf("Flashblocks replay anchor mismatch")
	}
	raw, header, err := prefix.rawBlock()
	if err != nil {
		return nil, nil, err
	}
	s := &snapshot{client: client, block: anchor, chain: chain, deployments: deployments}
	if err := s.checkCanonical(ctx); err != nil {
		return nil, nil, err
	}
	overlay, err := s.flashSystemState(ctx, prefix)
	if err != nil {
		return nil, nil, err
	}
	var traces []struct {
		Hash   string `json:"txHash"`
		Error  string `json:"error"`
		Result struct {
			Call flashCallFrame `json:"callTracer"`
			Diff stateDiff      `json:"prestateTracer"`
		} `json:"result"`
	}
	options := map[string]any{"tracer": "muxTracer", "tracerConfig": map[string]any{"callTracer": map[string]any{"withLog": true}, "prestateTracer": map[string]any{"diffMode": true}}, "timeout": "2s"}
	if err := client.Call(ctx, "debug_traceBlock", []any{flashHex(raw), options}, &traces); err != nil {
		return nil, nil, fmt.Errorf("native Flashblocks prefix replay: %w", err)
	}
	if len(traces) != len(prefix.Transactions) {
		return nil, nil, fmt.Errorf("Flashblocks replay returned incomplete transactions")
	}
	var gas uint64
	for i, trace := range traces {
		tx := prefix.Transactions[i]
		if trace.Error != "" || trace.Hash != tx.Hash.Hex() || (trace.Result.Call.Error == "") != tx.Successful {
			return nil, nil, fmt.Errorf("Flashblocks replay transaction %d identity/status mismatch", i)
		}
		used, err := quantity(trace.Result.Call.GasUsed)
		if err != nil || used > header.GasLimit-gas {
			return nil, nil, fmt.Errorf("Flashblocks replay gas mismatch")
		}
		gas += used
		logs, err := trace.Result.Call.logs()
		if err != nil {
			return nil, nil, err
		}
		if !equalFlashLogs(logs, tx.Logs) {
			return nil, nil, fmt.Errorf("Flashblocks replay transaction %d logs mismatch", i)
		}
		patch, err := trace.Result.Diff.overrides()
		if err != nil {
			return nil, nil, err
		}
		applyOverridePatch(overlay, patch)
	}
	// Jovian may account for a DA footprint in the block gas field. The signed
	// replay's execution gas must fit; the stream's larger gas use is conservative.
	if gas > header.GasUsed {
		return nil, nil, fmt.Errorf("Flashblocks stream understates replay gas")
	}
	if err := s.checkCanonical(ctx); err != nil {
		return nil, nil, err
	}
	return bindFlashSnapshot(client, anchor, chain, deployments, prefix, raw, header, overlay)
}

// Transaction tracers do not include the preceding system calls in their
// diffs. Reconstruct the two standardized post-Isthmus storage updates only
// after verifying their exact runtime bytecode at the parent. Upgrade blocks
// with absent/different system contracts are rejected.
func (s *snapshot) flashSystemState(ctx context.Context, prefix flashPrefix) (map[string]any, error) {
	e := prefix.Base.Execution
	result := make(map[string]any)
	for _, contract := range []struct {
		address domain.Address
		code    []byte
		storage map[string]string
	}{
		{domain.Address(params.BeaconRootsAddress), params.BeaconRootsCode, map[string]string{wordHex(prefix.Base.Timestamp % 8191): wordHex(prefix.Base.Timestamp), wordHex(prefix.Base.Timestamp%8191 + 8191): e.BeaconRoot.Hex()}},
		{domain.Address(params.HistoryStorageAddress), params.HistoryStorageCode, map[string]string{wordHex((prefix.Base.Number - 1) % 8191): prefix.Base.Parent.Hex()}},
	} {
		hash, err := s.CodeHash(ctx, contract.address)
		if err != nil {
			return nil, err
		}
		if hash != digest(contract.code) {
			return nil, fmt.Errorf("Flashblocks replay requires standard deployed beacon/history system code")
		}
		result[contract.address.Hex()] = map[string]any{"stateDiff": contract.storage}
	}
	return result, nil
}
func wordHex(value uint64) string { return fmt.Sprintf("0x%064x", value) }

type flashCallFrame struct {
	From    string           `json:"from"`
	Error   string           `json:"error"`
	GasUsed string           `json:"gasUsed"`
	Calls   []flashCallFrame `json:"calls"`
	Logs    []struct {
		Address  string         `json:"address"`
		Topics   []string       `json:"topics"`
		Data     string         `json:"data"`
		Position *tracePosition `json:"position"`
	} `json:"logs"`
}

func (f flashCallFrame) logs() ([]feed.FastLog, error) {
	result := []feed.FastLog{}
	if f.Error != "" {
		return result, nil
	}
	next := 0
	for i := 0; i <= len(f.Calls); i++ {
		for next < len(f.Logs) {
			entry := f.Logs[next]
			if entry.Position == nil || uint64(*entry.Position) > uint64(len(f.Calls)) || next > 0 && *entry.Position < *f.Logs[next-1].Position {
				return nil, fmt.Errorf("Flashblocks trace logs lack ordered call positions")
			}
			if uint64(*entry.Position) != uint64(i) {
				break
			}
			address, err := domain.ParseAddress(entry.Address)
			if err != nil {
				return nil, err
			}
			data, err := hexData(entry.Data)
			if err != nil || len(entry.Topics) > 4 {
				return nil, fmt.Errorf("Flashblocks replay has invalid log data")
			}
			log := feed.FastLog{Address: address, Data: data}
			for _, text := range entry.Topics {
				topic, err := domain.ParseHash(text)
				if err != nil {
					return nil, err
				}
				log.Topics = append(log.Topics, topic)
			}
			result = append(result, log)
			next++
		}
		if i < len(f.Calls) {
			logs, err := f.Calls[i].logs()
			if err != nil {
				return nil, err
			}
			result = append(result, logs...)
		}
	}
	return result, nil
}
func equalFlashLogs(a, b []feed.FastLog) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Address != b[i].Address || !bytes.Equal(a[i].Data, b[i].Data) || !slices.Equal(a[i].Topics, b[i].Topics) {
			return false
		}
	}
	return true
}

// Clients encode call-log positions as either integers or hex quantities.
type tracePosition uint64

func (p *tracePosition) UnmarshalJSON(raw []byte) error {
	var n uint64
	if len(raw) > 0 && raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		v, err := quantity(value)
		if err != nil {
			return err
		}
		n = v
	} else if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	*p = tracePosition(n)
	return nil
}
