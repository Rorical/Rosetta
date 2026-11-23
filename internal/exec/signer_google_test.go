package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"testing"
)

type secretRoundTripper func(*http.Request) (*http.Response, error)

func (f secretRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGoogleSigningSecretRequiresPinnedCanonicalVersion(t *testing.T) {
	const valid = "gcp://projects/123456789012/secrets/operator/versions/1"
	if name, err := googleSigningSecretName(valid); err != nil || name != strings.TrimPrefix(valid, "gcp://") {
		t.Fatal(name, err)
	}
	for _, ref := range []string{
		"gcp://projects/example-project/secrets/operator/versions/1",
		"gcp://projects/123456789012/secrets/operator/versions/latest",
		"gcp://projects/123456789012/secrets/operator/versions/0",
		"gcp://projects/123456789012/secrets/operator/versions/1?alt=media",
		"gcp://projects/123456789012/secrets/../versions/1",
		"gcp://projects/123456789012/secrets/operator/versions/1/extra",
	} {
		if _, err := googleSigningSecretName(ref); err == nil {
			t.Fatalf("accepted unpinned or malformed reference %q", ref)
		}
	}
}

func TestGoogleSigningSecretValidatesIdentityIntegrityAndErrorRedaction(t *testing.T) {
	const name = "projects/123456789012/secrets/operator/versions/1"
	payload := []byte("private-test-payload")
	checksum := fmt.Sprint(crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))
	for _, tc := range []struct {
		name, responseName string
		checksum           *string
		status             int
		wantOK             bool
	}{
		{"valid", name, &checksum, 200, true},
		{"wrong version", "projects/123456789012/secrets/operator/versions/2", &checksum, 200, false},
		{"wrong project", "projects/999999999999/secrets/operator/versions/1", &checksum, 200, false},
		{"missing checksum", name, nil, 200, false},
		{"corrupt payload", name, new(string), 200, false},
		{"forbidden", name, &checksum, 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := struct {
				Name    string `json:"name"`
				Payload struct {
					Data     []byte  `json:"data"`
					Checksum *string `json:"dataCrc32c,omitempty"`
				} `json:"payload"`
			}{Name: tc.responseName}
			response.Payload.Data, response.Payload.Checksum = payload, tc.checksum
			body, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: secretRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.String() != "https://secretmanager.googleapis.com/v1/"+name+":access" {
					t.Fatalf("unexpected secret request: %s %s", r.Method, r.URL)
				}
				if tc.status != 200 {
					body = payload
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
			})}
			got, err := readGoogleSigningSecret(context.Background(), client, name)
			if tc.wantOK {
				if err != nil || string(got) != string(payload) {
					t.Fatal("verified secret rejected", err)
				}
				return
			}
			if err == nil || len(got) != 0 {
				t.Fatal("unverified secret accepted")
			}
			if strings.Contains(err.Error(), string(payload)) {
				t.Fatal("secret leaked in error")
			}
		})
	}
}
