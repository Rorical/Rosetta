package rpc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func mustHash(t *testing.T, value string) domain.Hash {
	t.Helper()
	hash, err := domain.ParseHash(value)
	if err != nil {
		t.Fatalf("ParseHash(%q): %v", value, err)
	}
	return hash
}

func mustAddress(t *testing.T, value string) domain.Address {
	t.Helper()
	address, err := domain.ParseAddress(value)
	if err != nil {
		t.Fatalf("ParseAddress(%q): %v", value, err)
	}
	return address
}

type testRequest struct {
	ID     uint64 `json:"id"`
	Method string `json:"method"`
	Params []any  `json:"params"`
}

// decodeTestRequest reports whether the body decoded. Handlers must return
// after a false result instead of failing the test goroutine.
func decodeTestRequest(t *testing.T, r *http.Request) (testRequest, bool) {
	t.Helper()
	var request testRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode request: %v", err)
		return testRequest{}, false
	}
	return request, true
}

// resultServer answers every call with result.
func resultServer(t *testing.T, result any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeResult(t, w, request.ID, result)
	}))
	t.Cleanup(server.Close)
	return server
}
