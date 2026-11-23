package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
)

func (c cachedFlashState) matches(chainID uint64, p flashPrefix) error {
	e := p.Base.Execution
	if c.Index == nil || *c.Index != p.Index || c.PayloadID != p.Payload || c.StateOverrides == nil || len(c.Receipts) != len(p.Transactions) {
		return fmt.Errorf("Flashblocks cache prefix identity mismatch")
	}
	for _, field := range []struct {
		name, text string
		want       uint64
	}{
		{"chainId", c.ChainID, chainID}, {"blockNumber", c.BlockNumber, p.Base.Number},
		{"timestamp", c.Timestamp, p.Base.Timestamp}, {"gasLimit", c.GasLimit, e.GasLimit},
		{"gasUsed", c.GasUsed, *p.GasUsed},
	} {
		value, err := quantity(field.text)
		if err != nil || value != field.want {
			return fmt.Errorf("Flashblocks cache %s mismatch", field.name)
		}
	}
	for _, field := range []struct {
		name, text string
		want       domain.Hash
	}{
		{"parentHash", c.ParentHash, p.Base.Parent}, {"prevRandao", c.Random, e.Random},
		{"parentBeaconBlockRoot", c.BeaconRoot, e.BeaconRoot}, {"withdrawalsRoot", c.WithdrawalsRoot, *p.WithdrawalsRoot},
	} {
		value, err := domain.ParseHash(field.text)
		if err != nil || value != field.want {
			return fmt.Errorf("Flashblocks cache %s mismatch", field.name)
		}
	}
	fee, err := integer(c.BaseFee)
	if err != nil || fee.Cmp(e.BaseFee) != 0 {
		return fmt.Errorf("Flashblocks cache base fee mismatch")
	}
	coinbase, err := domain.ParseAddress(c.Coinbase)
	if err != nil || coinbase != e.Coinbase {
		return fmt.Errorf("Flashblocks cache fee recipient mismatch")
	}
	extra, err := hexData(c.Extra)
	if err != nil || !bytes.Equal(extra, e.Extra) {
		return fmt.Errorf("Flashblocks cache extra data mismatch")
	}
	if (c.BlobGasUsed == nil) != (p.BlobGasUsed == nil) {
		return fmt.Errorf("Flashblocks cache blob gas presence mismatch")
	}
	if c.BlobGasUsed != nil {
		gas, err := quantity(*c.BlobGasUsed)
		if err != nil || gas != *p.BlobGasUsed {
			return fmt.Errorf("Flashblocks cache blob gas mismatch")
		}
	}
	for i, tx := range p.Transactions {
		success, logs, err := decodeFlashRPCReceipt(c.Receipts[i], p.Base.Number, uint64(i), tx.Hash)
		if err != nil || success != tx.Successful || !equalFlashLogs(logs, tx.Logs) {
			return fmt.Errorf("Flashblocks cache transaction %d receipt mismatch", i)
		}
	}
	return nil
}

func decodeCachedOverrides(wire map[string]json.RawMessage) (map[string]any, error) {
	result := make(map[string]any, len(wire))
	for text, raw := range wire {
		address, err := domain.ParseAddress(text)
		if err != nil {
			return nil, err
		}
		if _, exists := result[address.Hex()]; exists {
			return nil, fmt.Errorf("Flashblocks cache repeats an account")
		}
		var entry struct {
			Balance *string           `json:"balance"`
			Nonce   *string           `json:"nonce"`
			Code    *string           `json:"code"`
			State   map[string]string `json:"state"`
			Diff    map[string]string `json:"stateDiff"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entry); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || entry.State != nil && entry.Diff != nil {
			return nil, fmt.Errorf("Flashblocks cache has invalid account override")
		}
		account := make(map[string]any)
		if entry.Balance != nil {
			balance, err := integer(*entry.Balance)
			if err != nil {
				return nil, err
			}
			account["balance"] = "0x" + balance.Text(16)
		}
		if entry.Nonce != nil {
			nonce, err := quantity(*entry.Nonce)
			if err != nil {
				return nil, err
			}
			account["nonce"] = fmt.Sprintf("0x%x", nonce)
		}
		if entry.Code != nil {
			code, err := hexData(*entry.Code)
			if err != nil {
				return nil, err
			}
			account["code"] = flashHex(code)
		}
		for key, storage := range map[string]map[string]string{"state": entry.State, "stateDiff": entry.Diff} {
			if storage == nil {
				continue
			}
			normalized := make(map[string]string, len(storage))
			for slot, value := range storage {
				if _, err := domain.ParseHash(slot); err != nil {
					return nil, err
				}
				if _, err := domain.ParseHash(value); err != nil {
					return nil, err
				}
				slot = strings.ToLower(slot)
				if _, exists := normalized[slot]; exists {
					return nil, fmt.Errorf("Flashblocks cache repeats a storage slot")
				}
				normalized[slot] = strings.ToLower(value)
			}
			account[key] = normalized
		}
		result[address.Hex()] = account
	}
	return result, nil
}
