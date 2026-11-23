package exec

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	keystoreReferencePrefix = "keystore://"
	keystorePasswordFileEnv = "ROSETTA_KEYSTORE_PASSWORD_FILE"
	keystorePasswordEnv     = "ROSETTA_KEYSTORE_PASSWORD"
)

// loadKeystoreSigner decrypts one local go-ethereum V3 key file. The decrypted
// scalar goes through newSecretSigner so its derived address must match the
// configured operator, the same check applied to cloud secrets.
func loadKeystoreSigner(reference string, expected domain.Address) (TransactionSigner, error) {
	path, err := keystoreFilePath(reference)
	if err != nil {
		return nil, err
	}
	password, err := keystorePassword()
	if err != nil {
		return nil, err
	}
	defer clear(password)
	keyJSON, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keystore file: %w", err)
	}
	key, err := keystore.DecryptKey(keyJSON, string(password))
	if err != nil {
		return nil, fmt.Errorf("decrypt keystore file: %w", err)
	}
	defer key.PrivateKey.D.SetInt64(0)
	return newSecretSigner(hex.EncodeToString(crypto.FromECDSA(key.PrivateKey)), expected)
}

// keystoreFilePath accepts a key file or a directory holding exactly one
// UTC--* key. An ambiguous directory is rejected instead of guessed.
func keystoreFilePath(reference string) (string, error) {
	path, ok := strings.CutPrefix(reference, keystoreReferencePrefix)
	if !ok || path == "" {
		return "", fmt.Errorf("keystore signer requires keystore://<path>")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat keystore path: %w", err)
	}
	if !info.IsDir() {
		return path, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("read keystore directory: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "UTC--") {
			files = append(files, filepath.Join(path, entry.Name()))
		}
	}
	if len(files) != 1 {
		return "", fmt.Errorf("keystore directory holds %d UTC-- key files; pass the keystore file path", len(files))
	}
	return files[0], nil
}

// keystorePassword reads the passphrase from the environment. The file form
// keeps it out of the unit environment; CR/LF left by editors and credential
// files is not part of the passphrase.
func keystorePassword() ([]byte, error) {
	if file := os.Getenv(keystorePasswordFileEnv); file != "" {
		password, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read keystore password file: %w", err)
		}
		return bytes.TrimRight(password, "\r\n"), nil
	}
	if value := os.Getenv(keystorePasswordEnv); value != "" {
		return []byte(value), nil
	}
	return nil, fmt.Errorf("keystore signer requires %s or %s", keystorePasswordFileEnv, keystorePasswordEnv)
}
