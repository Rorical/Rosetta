package telemetry

import (
	"net"
	"net/http"
	"testing"
)

func TestProfilingServesLoopbackOnlyWithoutCommandLine(t *testing.T) {
	if _, err := StartProfiling("0.0.0.0:0"); err == nil {
		t.Fatal("non-loopback profile listener accepted")
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()
	stop, err := StartProfiling(address)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for path, want := range map[string]int{"/debug/pprof/heap": http.StatusOK, "/debug/pprof/cmdline": http.StatusNotFound} {
		response, err := http.Get("http://" + address + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s: status %d, want %d", path, response.StatusCode, want)
		}
	}
}
