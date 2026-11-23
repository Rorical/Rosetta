package bot

import (
	"context"
	"errors"
	"flag"
	"io"
	"testing"

	"github.com/Rorical/Rosetta/internal/chain"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
)

type testFactory struct {
	create func(config.Config) (chain.Runtime, error)
}

func (f testFactory) Create(_ context.Context, cfg config.Config) (chain.Runtime, error) {
	return f.create(cfg)
}

type testRuntime struct {
	identity domain.ChainIdentity
	run      func() error
}

func (r testRuntime) Identity() domain.ChainIdentity { return r.identity }
func (r testRuntime) Run(context.Context) error      { return r.run() }

func TestRunHandlesCLIWithoutConstructingBackend(t *testing.T) {
	factory := testFactory{create: func(config.Config) (chain.Runtime, error) { t.Fatal("help constructed backend"); return nil, nil }}
	if err := Run(t.Context(), []string{"-h"}, io.Discard, factory); !errors.Is(err, flag.ErrHelp) {
		t.Fatal(err)
	}
}

func TestRunConfiguredChecksIdentityBeforeStartingAnyFamily(t *testing.T) {
	for _, family := range []domain.ChainFamily{domain.FamilyNitro, domain.FamilyOPStack} {
		t.Run(string(family), func(t *testing.T) {
			cfg, err := (config.Config{}).ResolveNetwork()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Network.Family = family
			expected := domain.ChainIdentity{Family: family, ChainID: cfg.ChainID()}
			for _, wrong := range []domain.ChainIdentity{{Family: family, ChainID: 99}, {Family: "wrong", ChainID: expected.ChainID}} {
				factory := testFactory{create: func(config.Config) (chain.Runtime, error) {
					return testRuntime{identity: wrong, run: func() error { t.Fatal("mismatched backend started"); return nil }}, nil
				}}
				if err := RunConfigured(t.Context(), cfg, factory); err == nil {
					t.Fatal("identity mismatch accepted")
				}
			}
			stopped := errors.New("backend stopped")
			factory := testFactory{create: func(selected config.Config) (chain.Runtime, error) {
				if selected.Network.Family != family {
					t.Fatal("family lost")
				}
				return testRuntime{identity: expected, run: func() error { return stopped }}, nil
			}}
			if err := RunConfigured(t.Context(), cfg, factory); !errors.Is(err, stopped) {
				t.Fatal("runtime result lost", err)
			}
		})
	}
}
