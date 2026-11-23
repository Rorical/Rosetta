package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Rorical/Rosetta/internal/domain"
)

type traceAccount struct {
	Balance *string           `json:"balance"`
	Nonce   *uint64           `json:"nonce"`
	Code    *string           `json:"code"`
	Storage map[string]string `json:"storage"`
}
type stateDiff struct{ Pre, Post map[string]traceAccount }

// mergeOverrides preserves earlier storage writes when a later trace changes
// another slot. An explicit full-state replacement resets the earlier diff.
func mergeOverrides(base, patch map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(patch))
	for address, raw := range base {
		account := make(map[string]any)
		for key, value := range raw.(map[string]any) {
			if key == "state" || key == "stateDiff" {
				copy := make(map[string]string)
				for k, v := range value.(map[string]string) {
					copy[k] = v
				}
				account[key] = copy
			} else {
				account[key] = value
			}
		}
		merged[address] = account
	}
	applyOverridePatch(merged, patch)
	return merged
}

// applyOverridePatch is for an owned replay buffer before publication. It
// avoids cloning the entire accumulated world state after every transaction.
func applyOverridePatch(merged, patch map[string]any) {
	for address, raw := range patch {
		account, ok := merged[address].(map[string]any)
		if !ok {
			account = make(map[string]any)
			merged[address] = account
		}
		for key, value := range raw.(map[string]any) {
			if key == "state" {
				delete(account, "stateDiff")
				copy := make(map[string]string)
				for k, v := range value.(map[string]string) {
					copy[k] = v
				}
				account[key] = copy
				continue
			}
			if key == "stateDiff" {
				destination := "stateDiff"
				if account["state"] != nil {
					destination = "state"
				}
				storage, ok := account[destination].(map[string]string)
				if !ok {
					storage = make(map[string]string)
					account[destination] = storage
				}
				for k, v := range value.(map[string]string) {
					storage[k] = v
				}
				continue
			}
			account[key] = value
		}
	}
}

func (d stateDiff) overrides() (map[string]any, error) {
	if d.Pre == nil || d.Post == nil {
		return nil, fmt.Errorf("remote post-state trace is incomplete")
	}
	result := make(map[string]any)
	for text, before := range d.Pre {
		if _, err := domain.ParseAddress(text); err != nil {
			return nil, err
		}
		after, exists := d.Post[text]
		if !exists {
			result[text] = map[string]any{"balance": "0x0", "nonce": "0x0", "code": "0x", "state": map[string]string{}}
			continue
		}
		account, err := after.override()
		if err != nil {
			return nil, err
		}
		storage := make(map[string]string)
		for key := range before.Storage {
			if _, err := domain.ParseHash(key); err != nil {
				return nil, err
			}
			storage[key] = domain.Hash{}.Hex()
		}
		for key, value := range after.Storage {
			storage[key] = value
		}
		if len(storage) > 0 {
			account["stateDiff"] = storage
		}
		result[text] = account
	}
	for text, after := range d.Post {
		if _, exists := d.Pre[text]; exists {
			continue
		}
		if _, err := domain.ParseAddress(text); err != nil {
			return nil, err
		}
		account, err := after.override()
		if err != nil {
			return nil, err
		}
		// A created account has empty storage outside the reported slots.
		delete(account, "stateDiff")
		account["state"] = after.Storage
		if after.Storage == nil {
			account["state"] = map[string]string{}
		}
		result[text] = account
	}
	return result, nil
}
func (a traceAccount) override() (map[string]any, error) {
	account := make(map[string]any)
	if a.Balance != nil {
		if _, err := integer(*a.Balance); err != nil {
			return nil, err
		}
		account["balance"] = *a.Balance
	}
	if a.Nonce != nil {
		account["nonce"] = "0x" + strconv.FormatUint(*a.Nonce, 16)
	}
	if a.Code != nil {
		if _, err := hexData(*a.Code); err != nil {
			return nil, err
		}
		account["code"] = *a.Code
	}
	for key, value := range a.Storage {
		if _, err := domain.ParseHash(key); err != nil {
			return nil, err
		}
		if _, err := domain.ParseHash(value); err != nil {
			return nil, err
		}
	}
	if len(a.Storage) > 0 {
		account["stateDiff"] = a.Storage
	}
	return account, nil
}

func (s *snapshot) traceCall(ctx context.Context, args map[string]any, options map[string]any, out any) error {
	if s.overlay != nil {
		patch, _ := options["stateOverrides"].(map[string]any)
		options["stateOverrides"] = mergeOverrides(s.overlay, patch)
		options["blockOverrides"] = s.blockOverrides
	}
	return s.client.Call(ctx, "debug_traceCall", []any{args, s.blockID(), options}, out)
}
func (s *snapshot) contractCall(ctx context.Context, to domain.Address, input []byte, patch map[string]any) ([]byte, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	factory := s.keepIdentity && patch == nil && s.overlay == nil && s.chain.ChainID != 0 && bytes.Equal(input, factorySelector)
	if factory {
		if output, ok := immutable.factory(s.chain.ChainID, to); ok {
			return bytes.Clone(output), nil
		}
	}
	params := s.contractParams(to, input, patch)
	output, err := s.readString(ctx, "eth_call", params)
	if err != nil {
		return nil, err
	}
	data, err := hexData(output)
	if err == nil && factory {
		immutable.keepFactory(s.chain.ChainID, to, data)
	}
	return data, err
}
func overlayHash(overlay map[string]any) (domain.Hash, error) {
	encoded, err := json.Marshal(overlay)
	return digest(encoded), err
}

func (s *snapshot) contractParams(to domain.Address, input []byte, patch map[string]any) []any {
	params := []any{map[string]any{"to": to.Hex(), "data": flashHex(input)}, s.blockID()}
	if s.overlay != nil || patch != nil {
		params = append(params, mergeOverrides(s.overlay, patch))
	}
	if s.blockOverrides != nil {
		params = append(params, s.blockOverrides)
	}
	return params
}
