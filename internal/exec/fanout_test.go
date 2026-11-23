package exec

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func sendServer(t *testing.T, delay time.Duration, body string) *rpc.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return rpc.NewClient(server.URL, rpc.Options{Timeout: 2 * time.Second})
}

func TestFanoutReturnsTheFirstAcknowledgementAndReportsEveryPath(t *testing.T) {
	hash := domain.Hash{0xab}
	ack := `{"jsonrpc":"2.0","id":1,"result":"` + hash.Hex() + `"}`
	known := `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"already known"}}`
	reports := make(chan []PathResult, 1)
	n := RPCDispatchNetwork{Submit: sendServer(t, 150*time.Millisecond, known), Fanout: []SubmitPath{{Name: "fast", Client: sendServer(t, 0, ack)}},
		OnPaths: func(_ domain.Hash, results []PathResult) { reports <- results }}
	started := time.Now()
	got, err := n.Send(t.Context(), []byte{1}, nil)
	if err != nil || got != hash || time.Since(started) > 100*time.Millisecond {
		t.Fatal("fan-out waited for the slow path or lost the fast acknowledgement", got, err, time.Since(started))
	}
	results := <-reports
	if len(results) != 2 || results[0].Name != "primary" || results[0].Accepted || !strings.Contains(results[0].Error, "already known") || !results[1].Accepted {
		t.Fatalf("path report: %+v", results)
	}

	// The primary refusing is not a failure while another path delivered.
	n.Submit = sendServer(t, 0, known)
	n.Fanout = []SubmitPath{{Name: "fast", Client: sendServer(t, 20*time.Millisecond, ack)}}
	if got, err := n.Send(t.Context(), []byte{1}, nil); err != nil || got != hash {
		t.Fatal("a delivering secondary path was ignored", err)
	}
	<-reports

	// Every path failing is an error, and transport details never leak URLs.
	n.Submit = rpc.NewClient("http://127.0.0.1:1/secret-key", rpc.Options{Timeout: time.Second})
	n.Fanout = []SubmitPath{{Name: "down", Client: rpc.NewClient("http://127.0.0.1:1/other-key", rpc.Options{Timeout: time.Second})}}
	if _, err := n.Send(t.Context(), []byte{1}, nil); err == nil {
		t.Fatal("all paths failed without an error")
	}
	for _, r := range <-reports {
		if r.Accepted || strings.Contains(r.Error, "key") {
			t.Fatalf("failed path report leaked or misreported: %+v", r)
		}
	}
}
