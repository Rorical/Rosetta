package exec

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// TransactionSigner signs only envelopes authorized by the dispatch gate.
// Implementations must return the same transaction and the configured sender.
type TransactionSigner interface {
	Address() domain.Address
	Sign(context.Context, *types.Transaction) (*types.Transaction, error)
}

type secretSigner struct {
	key     *ecdsa.PrivateKey
	address domain.Address
}

// LoadSecretSigner loads a hex private scalar once. A gcp:// reference selects
// a pinned Google secret version, a keystore:// reference decrypts a local
// go-ethereum key file, and other IDs use AWS Secrets Manager. Cloud
// credentials use the provider's default chain. Wallet keys stay in memory.
func LoadSecretSigner(ctx context.Context, secretID string, expected domain.Address) (TransactionSigner, error) {
	if secretID == "" || expected == (domain.Address{}) {
		return nil, fmt.Errorf("signer requires a secret ID and expected address")
	}
	if strings.HasPrefix(secretID, "gcp://") {
		return loadGoogleSecretSigner(ctx, secretID, expected)
	}
	if strings.HasPrefix(secretID, keystoreReferencePrefix) {
		return loadKeystoreSigner(secretID, expected)
	}
	if strings.Contains(secretID, "://") {
		return nil, fmt.Errorf("unsupported signing secret reference")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load signer credentials: %w", err)
	}
	out, err := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretID})
	if err != nil {
		return nil, fmt.Errorf("read signer secret: %w", err)
	}
	if out.SecretString == nil {
		return nil, fmt.Errorf("signer secret must be a hex SecretString")
	}
	return newSecretSigner(*out.SecretString, expected)
}

func newSecretSigner(encoded string, expected domain.Address) (TransactionSigner, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(encoded), "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid signing key encoding")
	}
	address := domain.Address(crypto.PubkeyToAddress(key.PublicKey))
	if address != expected {
		key.D.SetInt64(0)
		return nil, fmt.Errorf("signer address differs from configured operator")
	}
	return &secretSigner{key: key, address: address}, nil
}
func (s *secretSigner) Address() domain.Address { return s.address }
func (s *secretSigner) Sign(ctx context.Context, tx *types.Transaction) (*types.Transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return types.SignTx(tx, types.LatestSignerForChainID(tx.ChainId()), s.key)
}

func verifySigned(unsigned, signed *types.Transaction, sender domain.Address) error {
	if signed == nil || unsigned.Type() != types.DynamicFeeTxType || signed.Type() != unsigned.Type() || signed.ChainId().Cmp(unsigned.ChainId()) != 0 {
		return fmt.Errorf("signer changed transaction type or chain")
	}
	signer := types.LatestSignerForChainID(unsigned.ChainId())
	if signer.Hash(unsigned) != signer.Hash(signed) {
		return fmt.Errorf("signer changed authorized envelope")
	}
	got, err := types.Sender(signer, signed)
	if err != nil || got != common.Address(sender) {
		return fmt.Errorf("signature sender mismatch")
	}
	return nil
}
