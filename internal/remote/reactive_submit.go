package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// submitWarmInterval keeps the submission connection busier than Go's 90 s
// idle-connection timeout. Intents arrive minutes apart; a cold connection
// added about 170 ms of DNS, TCP and TLS setup to the first live send.
const submitWarmInterval = 30 * time.Second

// submitTimeout bounds one send. A Nitro sequencer answers only once the
// transaction is in a block, so an answer can take over 2 s and the send
// still lands, and a timed-out send keeps the nonce lane busy
// as an unknown outcome until a canonical sighting.
const submitTimeout = 5 * time.Second

// openSubmitClient connects to the submission endpoint, proves it serves the
// expected chain, and keeps that connection warm until ctx ends. eth_chainId
// is answered by the sequencer's proxy without side effects.
// The client is isolated from the runtime's read pacing: queued reads used to
// hold sends for 200-600 ms.
func openSubmitClient(ctx context.Context, endpoint, dial string, chainID uint64, interval time.Duration) (*rpc.Client, error) {
	client := rpc.NewClient(endpoint, rpc.Options{Timeout: submitTimeout, Isolated: true, DialAddress: dial})
	if err := checkSubmitChain(ctx, client, chainID); errors.Is(err, errSubmitOnly) {
		slog.Warn("submission endpoint does not name its chain; signatures bind the chain ID")
	} else if err != nil {
		return nil, err
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Any JSON-RPC reply, a submit-only endpoint's -32601 included,
				// has used and kept the connection.
				if err := checkSubmitChain(ctx, client, chainID); err != nil && !errors.Is(err, errSubmitOnly) && ctx.Err() == nil {
					slog.Warn("submission endpoint keepalive failed", "error", err)
				}
			}
		}
	}()
	return client, nil
}

// errSubmitOnly marks an endpoint that serves only eth_sendRawTransaction,
// as Robinhood's official sequencer does: eth_chainId is -32601.
var errSubmitOnly = errors.New("submission endpoint serves only transactions")

func checkSubmitChain(ctx context.Context, client *rpc.Client, chainID uint64) error {
	var text string
	if err := client.Call(ctx, "eth_chainId", []any{}, &text); err != nil {
		var reply *rpc.RPCError
		if errors.As(err, &reply) && reply.Code == -32601 {
			return errSubmitOnly
		}
		return fmt.Errorf("submission endpoint: %w", err)
	}
	id, err := quantity(text)
	if err != nil || id != chainID {
		return fmt.Errorf("submission endpoint serves chain %s, want %d", text, chainID)
	}
	return nil
}

// openFanout opens the extra submission paths named in a comma-separated list.
// Each is kept warm like the primary; one that fails its chain check is left
// out rather than blocking live startup. Records name a path by host only, or
// host@ip for a path pinned with "#ip=<address>": one name can front several
// sequencer addresses, and the same signed bytes sent to each let the first
// to arrive count (Robinhood's has three).
func openFanout(ctx context.Context, list string, chainID uint64) ([]execution.SubmitPath, func(domain.Hash, []execution.PathResult)) {
	var paths []execution.SubmitPath
	for _, endpoint := range strings.Split(list, ",") {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		endpoint, dial, err := pinnedEndpoint(endpoint)
		if err != nil {
			slog.Warn("fan-out submission path left out", "error", err)
			continue
		}
		name := "unparsed"
		if parsed, err := url.Parse(endpoint); err == nil && parsed.Hostname() != "" {
			name = parsed.Hostname()
			if ip, _, err := net.SplitHostPort(dial); err == nil {
				name += "@" + ip
			}
		}
		client, err := openSubmitClient(ctx, endpoint, dial, chainID, submitWarmInterval)
		if err != nil {
			slog.Warn("fan-out submission path left out", "path", name)
			continue
		}
		paths = append(paths, execution.SubmitPath{Name: name, Client: client})
	}
	if len(paths) == 0 {
		return nil, nil
	}
	return paths, func(hash domain.Hash, results []execution.PathResult) {
		slog.Info("submission paths", "hash", hash.Hex(), "paths", results)
	}
}

// pinnedEndpoint splits an optional "#ip=<address>" fragment off a submission
// URL and returns the URL without it and the host:port to dial; the URL's
// port, or its scheme's default, is kept.
func pinnedEndpoint(endpoint string) (string, string, error) {
	base, fragment, found := strings.Cut(endpoint, "#")
	if !found {
		return endpoint, "", nil
	}
	value, ok := strings.CutPrefix(fragment, "ip=")
	ip := net.ParseIP(value)
	parsed, err := url.Parse(base)
	if !ok || ip == nil || err != nil || parsed.Hostname() == "" {
		return "", "", fmt.Errorf("submission endpoint fragment must be ip=<address>")
	}
	port := parsed.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[parsed.Scheme]
	}
	if port == "" {
		return "", "", fmt.Errorf("submission endpoint scheme %q has no default port", parsed.Scheme)
	}
	return base, net.JoinHostPort(ip.String(), port), nil
}
