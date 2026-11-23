//go:build opnative && contracts

package remote

import (
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestCachedFlashStateMatchesNativePrefixConfirmation(t *testing.T) {
	for _, fork := range []string{"isthmus", "jovian"} {
		t.Run(fork, func(t *testing.T) {
			f := newOPFixture(t, fork)
			request := f.request(t)
			prefix, _ := nativeFlashPrefix(t, f)
			chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 8453}
			reference := &flashRPC{base: &opRPC{fixture: f, replay: true}}
			client := reference.server(t)
			parent, err := client.ExecutionBlock(t.Context(), "latest")
			if err != nil {
				t.Fatal(err)
			}
			// Materialize the node's cached result once. The client under test
			// receives only that result, with no prefix execution on its path.
			replayed, replayProof, err := openFlashSnapshot(t.Context(), client, parent, chain, domain.Deployments{}, prefix)
			if err != nil {
				t.Fatal(err)
			}
			defer replayed.Close()
			want, err := replayed.Confirm(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			cached := cacheExport(prefix, replayed.overlay)
			node := &flashRPC{base: &opRPC{fixture: f, replay: true}, cached: &cached}
			source := &flashSnapshotSource{}
			view, proof, err := source.open(t.Context(), node.server(t), parent, chain, domain.Deployments{}, prefix)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			got, err := view.Confirm(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) || proof.OverlayHash != replayProof.OverlayHash || proof.StateSource != "rpc-cache-v1" || node.nativeTraces.Load() != 0 {
				t.Fatal("cached execution differs from native prefix or replayed again", got, want, proof, node.nativeTraces.Load())
			}
			// The exported overlay remains an immutable private base even if a
			// retained reader runs another complete invocation.
			retained := view.Retain()
			view.Close()
			defer retained.Close()
			if again, err := retained.Confirm(t.Context(), request); err != nil || !reflect.DeepEqual(got, again) {
				t.Fatal("cache lease lost snapshot isolation", again, err)
			}
		})
	}
}
