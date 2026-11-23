package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"golang.org/x/oauth2/google"
)

// Use the canonical project number and a numeric version so the access response
// can be matched exactly. Aliases would allow a later rotation to change a key.
func googleSigningSecretName(reference string) (string, error) {
	name, ok := strings.CutPrefix(reference, "gcp://")
	parts := strings.Split(name, "/")
	if !ok || len(parts) != 6 || parts[0] != "projects" || parts[2] != "secrets" || parts[4] != "versions" {
		return "", fmt.Errorf("Google signer requires gcp://projects/PROJECT_NUMBER/secrets/SECRET/versions/VERSION")
	}
	for _, index := range []int{1, 5} {
		n, err := strconv.ParseUint(parts[index], 10, 64)
		if err != nil || n == 0 || strconv.FormatUint(n, 10) != parts[index] {
			return "", fmt.Errorf("Google signer requires a canonical project number and pinned numeric version")
		}
	}
	if parts[3] == "" {
		return "", fmt.Errorf("Google signing secret name is empty")
	}
	for _, c := range parts[3] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", fmt.Errorf("invalid Google signing secret name")
		}
	}
	return name, nil
}

func loadGoogleSecretSigner(ctx context.Context, reference string, expected domain.Address) (TransactionSigner, error) {
	name, err := googleSigningSecretName(reference)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("load Google signer credentials: %w", err)
	}
	defer client.CloseIdleConnections()
	payload, err := readGoogleSigningSecret(ctx, client, name)
	if err != nil {
		return nil, err
	}
	defer clear(payload)
	return newSecretSigner(string(payload), expected)
}

func readGoogleSigningSecret(ctx context.Context, client *http.Client, name string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://secretmanager.googleapis.com/v1/"+name+":access", nil)
	if err != nil {
		return nil, fmt.Errorf("construct Google signing secret request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read Google signing secret: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Google signing secret access returned HTTP %d", response.StatusCode)
	}
	// A hex signing scalar and its resource metadata fit well below this limit.
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return nil, fmt.Errorf("read Google signing secret response: %w", err)
	}
	defer clear(body)
	if len(body) > 4096 {
		return nil, fmt.Errorf("Google signing secret response exceeds signing key limit")
	}
	var secret struct {
		Name    string `json:"name"`
		Payload struct {
			Data     []byte  `json:"data"`
			Checksum *string `json:"dataCrc32c"`
		} `json:"payload"`
	}
	if err = json.Unmarshal(body, &secret); err != nil {
		clear(secret.Payload.Data)
		return nil, fmt.Errorf("invalid Google signing secret response")
	}
	if secret.Name != name || secret.Payload.Checksum == nil {
		clear(secret.Payload.Data)
		return nil, fmt.Errorf("Google signing secret identity or checksum missing")
	}
	expected, err := strconv.ParseUint(*secret.Payload.Checksum, 10, 32)
	actual := crc32.Checksum(secret.Payload.Data, crc32.MakeTable(crc32.Castagnoli))
	if err != nil || uint64(actual) != expected {
		clear(secret.Payload.Data)
		return nil, fmt.Errorf("Google signing secret checksum mismatch")
	}
	return secret.Payload.Data, nil
}
