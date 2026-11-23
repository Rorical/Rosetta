package exec

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

// Fee states of a booked settlement. An empty FeeStatus means Fees holds the
// paid amounts from a receipt.
const (
	FeesPending = "pending" // outcome known, receipt fees not yet read
	FeesUnknown = "unknown" // no receipt within the fee bound; the reservation stays charged
)

// Settlement is a nonce's booked outcome. Profit is the executor's after-loan
// token payout; Net uses the intent's frozen valuation and is labeled as such.
// Net stays nil until paid fees are known.
type Settlement struct {
	TransferDiagnosis string `json:",omitempty"`
	Cancelled         bool   `json:",omitempty"`
	// Maintenance names an owner sweep or unwrap: no arbitrage outcome, and
	// Net is minus the paid native fee.
	Maintenance          string          `json:",omitempty"`
	QuoteError           *big.Int        `json:",omitempty"`
	Intent               *ReactiveIntent `json:",omitempty"`
	Hash                 domain.Hash
	IntentID             domain.Hash
	Block                domain.BlockRef
	Position             *ExecutionPosition `json:",omitempty"` // feed sighting
	Success              bool
	Asset                domain.Address
	Profit, LoanFee, Net *big.Int
	Fees                 *domain.ReceiptFees
	FeeStatus            string `json:",omitempty"`
	Basis                string
}

type RPCDispatchNetwork struct {
	TokenRisks *tokenrisk.Registry
	// ProtectedTokens are never recorded as risks (start assets, wrapped
	// native): a revert trace runs code the route does not trust.
	ProtectedTokens map[domain.Address]bool
	Trace           *rpc.Client
	Read, Submit    *rpc.Client
	Chain           domain.ChainIdentity
	// Fanout receives every signed transaction alongside Submit; the first
	// acknowledgement wins. OnPaths gets each path's answer once all return.
	Fanout  []SubmitPath
	OnPaths func(domain.Hash, []PathResult)
}

func (n RPCDispatchNetwork) PendingNonce(ctx context.Context, sender domain.Address) (uint64, error) {
	var id, nonce string
	if err := n.Read.Call(ctx, "eth_chainId", []any{}, &id); err != nil {
		return 0, err
	}
	value, ok := new(big.Int).SetString(id, 0)
	if !ok || !value.IsUint64() || value.Uint64() != n.Chain.ChainID {
		return 0, fmt.Errorf("dispatch RPC chain mismatch")
	}
	if err := n.Read.Call(ctx, "eth_getTransactionCount", []any{sender.Hex(), "pending"}, &nonce); err != nil {
		return 0, err
	}
	value, ok = new(big.Int).SetString(nonce, 0)
	if !ok || !value.IsUint64() || value.Uint64() == ^uint64(0) {
		return 0, fmt.Errorf("invalid pending nonce")
	}
	return value.Uint64(), nil
}
func (n RPCDispatchNetwork) ConfirmedNonce(ctx context.Context, sender domain.Address) (uint64, error) {
	var result string
	if err := n.Read.Call(ctx, "eth_getTransactionCount", []any{sender.Hex(), "latest"}, &result); err != nil {
		return 0, err
	}
	value, ok := new(big.Int).SetString(result, 0)
	if !ok || !value.IsUint64() {
		return 0, fmt.Errorf("invalid latest nonce")
	}
	return value.Uint64(), nil
}
func (n RPCDispatchNetwork) Send(ctx context.Context, raw []byte, conditions *TransactionConditions) (domain.Hash, error) {
	encoded := "0x" + hex.EncodeToString(raw)
	if conditions != nil {
		// Only the sequencer evaluates conditions; fan-out paths do not get them.
		var hash string
		if err := n.Submit.Call(ctx, "eth_sendRawTransactionConditional", []any{encoded, conditions.options()}, &hash); err != nil {
			var reply *rpc.RPCError
			if errors.As(err, &reply) && reply.Code == conditionsRejectedCode {
				return domain.Hash{}, fmt.Errorf("%w: %v", ErrConditionsRejected, err)
			}
			return domain.Hash{}, err
		}
		return domain.ParseHash(hash)
	}
	if len(n.Fanout) > 0 {
		return n.fanout(ctx, raw)
	}
	var hash string
	if err := n.Submit.Call(ctx, "eth_sendRawTransaction", []any{encoded}, &hash); err != nil {
		return domain.Hash{}, err
	}
	return domain.ParseHash(hash)
}

// conditionsRejectedCode is Nitro's answer to a conditional transaction whose
// conditions failed, on admission or at its execution position.
const conditionsRejectedCode = -32003

func (n RPCDispatchNetwork) Receipt(ctx context.Context, hash domain.Hash) (*rpc.TransactionReceipt, error) {
	return n.Read.TransactionReceipt(ctx, hash, n.Chain.Family)
}

// Diagnose traces a reverted executor call once. A registry write failure is
// reported in the diagnosis; it never changes the booked settlement.
func (n RPCDispatchNetwork) Diagnose(ctx context.Context, intent ReactiveIntent, settled *Settlement) string {
	if n.Trace == nil && n.Read == nil {
		return "trace_unavailable"
	}
	diagnosed := *settled
	if err := n.observeFailedTransfer(ctx, intent, &diagnosed); err != nil {
		return "transfer_shortfall_record_failed: " + err.Error()
	}
	return diagnosed.TransferDiagnosis
}

func settleExecutor(p PendingTransaction, r *rpc.TransactionReceipt) (*Settlement, error) {
	s := &Settlement{Intent: &p.Intent, Hash: r.Hash, IntentID: p.Intent.ID, Block: r.Block, Success: r.Success, Asset: p.Intent.Route.Asset, Profit: new(big.Int), LoanFee: new(big.Int), Fees: r.Fees, Basis: "canonical_receipt; admission_conversion"}
	if p.Cancellation != nil {
		s.Cancelled = true
		s.Basis = "canonical_cancellation; admission_conversion"
	}
	if p.Maintenance != nil {
		s.Maintenance, s.Basis = p.Maintenance.Kind, "canonical_maintenance; native"
	}
	if r.Success && !s.Cancelled && s.Maintenance == "" {
		var err error
		s.Profit, s.LoanFee, err = executorOutcome(p, r.Logs)
		if err != nil {
			return nil, err
		}
	}
	if r.Success && !s.Cancelled && s.Maintenance == "" && p.Intent.QuotedGross != nil {
		s.QuoteError = new(big.Int).Add(s.Profit, s.LoanFee)
		s.QuoteError.Sub(s.QuoteError, p.Intent.QuotedGross)
	}
	if err := s.price(p.Intent.Conversion); err != nil {
		return nil, err
	}
	return s, nil
}

// price sets Net from paid fees, or marks the fees pending when there are none.
func (s *Settlement) price(conversion FeeConversion) error {
	if s.Fees == nil || s.Fees.Total == nil {
		s.Fees, s.Net, s.FeeStatus = nil, nil, FeesPending
		return nil
	}
	fee, err := FeeInAsset(s.Fees.Total, conversion)
	if err != nil {
		return err
	}
	s.Net = new(big.Int).Sub(s.Profit, fee)
	s.FeeStatus = ""
	return nil
}
