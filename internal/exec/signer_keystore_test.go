package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/ethereum/go-ethereum/accounts/keystore"
)

const keystoreTestPassword = "test-keystore-passphrase"

func newKeystoreFixture(t *testing.T, accounts int) (string, domain.Address) {
	t.Helper()
	dir := t.TempDir()
	store := keystore.NewKeyStore(dir, keystore.LightScryptN, keystore.LightScryptP)
	var address domain.Address
	for i := 0; i < accounts; i++ {
		account, err := store.NewAccount(keystoreTestPassword)
		if err != nil {
			t.Fatalf("create keystore account: %v", err)
		}
		if i == 0 {
			address = domain.Address(account.Address)
		}
	}
	return dir, address
}

// writeKeystorePasswordFile includes a trailing CRLF so the loader's trim is
// exercised, as a credential file written by an editor would.
func writeKeystorePasswordFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, []byte(keystoreTestPassword+"\r\n"), 0o600); err != nil {
		t.Fatalf("write keystore password file: %v", err)
	}
	return path
}

func setKeystorePasswordEnv(t *testing.T, file, value string) {
	t.Helper()
	t.Setenv(keystorePasswordFileEnv, file)
	t.Setenv(keystorePasswordEnv, value)
}

func TestLoadKeystoreSignerReadsDirectoryAndFile(t *testing.T) {
	dir, address := newKeystoreFixture(t, 1)
	files, err := filepath.Glob(filepath.Join(dir, "UTC--*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("test keystore files = %v, %v", files, err)
	}
	setKeystorePasswordEnv(t, writeKeystorePasswordFile(t), "")
	for _, reference := range []string{"keystore://" + dir, "keystore://" + files[0]} {
		signer, err := LoadSecretSigner(context.Background(), reference, address)
		if err != nil {
			t.Fatalf("load %q: %v", reference, err)
		}
		if signer.Address() != address {
			t.Fatalf("%q address = %x, want %x", reference, signer.Address(), address)
		}
	}
}

func TestLoadKeystoreSignerReadsPasswordEnvironment(t *testing.T) {
	dir, address := newKeystoreFixture(t, 1)
	setKeystorePasswordEnv(t, "", keystoreTestPassword)
	signer, err := LoadSecretSigner(context.Background(), "keystore://"+dir, address)
	if err != nil {
		t.Fatalf("load keystore: %v", err)
	}
	if signer.Address() != address {
		t.Fatalf("address = %x, want %x", signer.Address(), address)
	}
}

func TestLoadKeystoreSignerRejectsWrongPassword(t *testing.T) {
	const wrong = "not-the-passphrase"
	dir, address := newKeystoreFixture(t, 1)
	setKeystorePasswordEnv(t, "", wrong)
	_, err := LoadSecretSigner(context.Background(), "keystore://"+dir, address)
	if err == nil {
		t.Fatal("keystore accepted a wrong password")
	}
	if strings.Contains(err.Error(), wrong) {
		t.Fatal("password appears in error")
	}
}

func TestLoadKeystoreSignerRejectsExpectedAddressMismatch(t *testing.T) {
	dir, _ := newKeystoreFixture(t, 1)
	setKeystorePasswordEnv(t, writeKeystorePasswordFile(t), "")
	if _, err := LoadSecretSigner(context.Background(), "keystore://"+dir, domain.Address{1}); err == nil {
		t.Fatal("keystore accepted an unexpected address")
	}
}

func TestLoadKeystoreSignerRejectsAmbiguousDirectory(t *testing.T) {
	dir, address := newKeystoreFixture(t, 2)
	setKeystorePasswordEnv(t, writeKeystorePasswordFile(t), "")
	_, err := LoadSecretSigner(context.Background(), "keystore://"+dir, address)
	if err == nil || !strings.Contains(err.Error(), "pass the keystore file path") {
		t.Fatalf("error = %v, want file path guidance", err)
	}
}

func TestLoadKeystoreSignerRequiresPasswordSource(t *testing.T) {
	dir, address := newKeystoreFixture(t, 1)
	setKeystorePasswordEnv(t, "", "")
	_, err := LoadSecretSigner(context.Background(), "keystore://"+dir, address)
	if err == nil {
		t.Fatal("keystore loaded without a password source")
	}
	for _, name := range []string{keystorePasswordFileEnv, keystorePasswordEnv} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not name %s", err, name)
		}
	}
}

func TestLoadKeystoreSignerRequiresPath(t *testing.T) {
	if _, err := LoadSecretSigner(context.Background(), "keystore://", domain.Address{1}); err == nil {
		t.Fatal("keystore reference without a path was accepted")
	}
}
