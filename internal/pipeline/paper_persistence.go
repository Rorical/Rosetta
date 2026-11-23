package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

type savedPaidSample struct {
	Shape       [17]byte        `json:"shape"`
	Block       domain.BlockRef `json:"block"`
	Transaction domain.Hash     `json:"transaction"`
	Total       *big.Int        `json:"total"`
}
type savedQuoteSample struct {
	Block       uint64      `json:"block"`
	Pips        uint32      `json:"pips"`
	Transaction domain.Hash `json:"transaction"`
	LogIndex    int         `json:"log_index"`
}
type savedDirection struct {
	Pool    domain.PoolKey     `json:"pool"`
	Token   domain.Address     `json:"token"`
	Samples []savedQuoteSample `json:"samples"`
}
type savedPaperBook struct {
	Version int                  `json:"version"`
	Chain   domain.ChainIdentity `json:"chain"`
	Wrapped domain.Address       `json:"wrapped_native"`
	Head    domain.BlockRef      `json:"head"`
	Headers []domain.BlockRef    `json:"headers"`
	Paid    []savedPaidSample    `json:"paid"`
	Quotes  []savedDirection     `json:"quotes"`
}

// Only the research worker writes this file. Pool writers merely update the
// in-memory book. Rename and directory sync leave one complete checkpoint.
func (b *paperBook) persist(chain domain.ChainIdentity, previous uint64) (uint64, error) {
	if b.policy.SamplePath == "" {
		return previous, nil
	}
	b.mu.RLock()
	if b.revision == previous || b.pending {
		b.mu.RUnlock()
		return previous, nil
	}
	revision := b.revision
	wrapped, _ := domain.ParseAddress(b.policy.WrappedNative)
	snapshot := savedPaperBook{Version: 1, Chain: chain, Wrapped: wrapped, Head: b.head}
	for _, header := range b.headers {
		snapshot.Headers = append(snapshot.Headers, header)
	}
	slices.SortFunc(snapshot.Headers, func(a, c domain.BlockRef) int {
		if a.Number < c.Number {
			return -1
		}
		if a.Number > c.Number {
			return 1
		}
		return 0
	})
	for _, sample := range b.paid {
		snapshot.Paid = append(snapshot.Paid, savedPaidSample{sample.shape, sample.block, sample.tx, new(big.Int).Set(sample.fees.Total)})
	}
	for key, samples := range b.quotes {
		row := savedDirection{Pool: key.Pool, Token: key.TokenIn}
		for _, s := range samples {
			row.Samples = append(row.Samples, savedQuoteSample{s.block, s.pips, s.tx, s.logIndex})
		}
		snapshot.Quotes = append(snapshot.Quotes, row)
	}
	b.mu.RUnlock()
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return previous, fmt.Errorf("encode paper samples: %w", err)
	}
	if err = writePaperSnapshot(b.policy.SamplePath, encoded); err != nil {
		return previous, err
	}
	return revision, nil
}

func writePaperSnapshot(path string, encoded []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create paper sample directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".paper-samples-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(encoded); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func (b *paperBook) restore(chain domain.ChainIdentity) error {
	if b.policy.SamplePath == "" {
		return nil
	}
	if chain.ChainID == 0 || chain.Family == "" {
		return fmt.Errorf("paper sample persistence requires chain identity")
	}
	file, err := os.Open(b.policy.SamplePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var snapshot savedPaperBook
	decoder := json.NewDecoder(io.LimitReader(file, 64<<20))
	if err = decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("decode paper samples: %w", err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("paper samples contain trailing data")
	}
	wrapped, _ := domain.ParseAddress(b.policy.WrappedNative)
	if snapshot.Version != 1 || snapshot.Chain != chain || snapshot.Wrapped != wrapped {
		return fmt.Errorf("paper sample identity mismatch")
	}
	if len(snapshot.Headers) > 65537 || len(snapshot.Paid) > 2048 || len(snapshot.Quotes) > 4096 {
		return fmt.Errorf("paper sample limits exceeded")
	}
	loaded := newPaperBook(b.policy)
	loaded.head = snapshot.Head
	for i, header := range snapshot.Headers {
		if header.Hash == (domain.Hash{}) || header.Number > snapshot.Head.Number {
			return fmt.Errorf("invalid paper sample header")
		}
		if i > 0 {
			previous := snapshot.Headers[i-1]
			if header.Number != previous.Number+1 || header.ParentHash != previous.Hash {
				return fmt.Errorf("paper sample headers are not contiguous")
			}
		}
		loaded.headers[header.Number] = header
	}
	if loaded.headers[snapshot.Head.Number] != snapshot.Head || snapshot.Head.Hash == (domain.Hash{}) {
		return fmt.Errorf("paper sample head missing")
	}
	seenPaid := map[domain.Hash]bool{}
	for _, sample := range snapshot.Paid {
		// Files written before paid samples took the book's header can hold a
		// block named by number and hash alone; the hash is its identity.
		header, ok := loaded.headers[sample.Block.Number]
		if sample.Shape[0] < 1 || sample.Shape[0] > 16 || sample.Total == nil || sample.Total.Sign() < 0 || sample.Total.BitLen() > 256 || sample.Transaction == (domain.Hash{}) || seenPaid[sample.Transaction] || !ok || header.Hash != sample.Block.Hash {
			return fmt.Errorf("invalid paid sample")
		}
		for i, kind := range sample.Shape[1:] {
			if i < int(sample.Shape[0]) {
				if kind < byte(domain.PoolKindV2) || kind > byte(domain.PoolKindMetricOmm) {
					return fmt.Errorf("invalid paid protocol")
				}
			} else if kind != 0 {
				return fmt.Errorf("invalid paid shape")
			}
		}
		seenPaid[sample.Transaction] = true
		loaded.paid = append(loaded.paid, paidSample{sample.Shape, header, sample.Transaction, &domain.ReceiptFees{Total: sample.Total}})
	}
	for _, row := range snapshot.Quotes {
		key := quoteDirection{row.Pool, row.Token}
		if row.Pool.Address == (domain.Address{}) || row.Token == (domain.Address{}) && !row.Pool.IsV4() || len(row.Samples) > 64 || loaded.quotes[key] != nil {
			return fmt.Errorf("invalid quote sample direction")
		}
		for _, sample := range row.Samples {
			if _, ok := loaded.headers[sample.Block]; !ok || sample.Pips > 1000000 || sample.LogIndex < 0 {
				return fmt.Errorf("invalid quote sample")
			}
			loaded.quotes[key] = append(loaded.quotes[key], quoteSample{sample.Block, sample.Pips, sample.Transaction, sample.LogIndex})
		}
	}
	loaded.prune()
	loaded.limitQuotes()
	b.head, b.headers, b.paid, b.quotes = loaded.head, loaded.headers, loaded.paid, loaded.quotes
	b.pending = true
	return nil
}
