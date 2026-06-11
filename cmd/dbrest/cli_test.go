package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/tamnd/dbrest/adminapi"
	"github.com/tamnd/dbrest/config"
)

// TestResolveConfigPath covers the positional/flag reconciliation: either
// spelling alone, agreement, disagreement, and too many arguments.
func TestResolveConfigPath(t *testing.T) {
	cases := []struct {
		flag    string
		args    []string
		want    string
		wantErr bool
	}{
		{"", nil, "", false},
		{"a.conf", nil, "a.conf", false},
		{"", []string{"b.conf"}, "b.conf", false},
		{"a.conf", []string{"a.conf"}, "a.conf", false},
		{"a.conf", []string{"b.conf"}, "", true},
		{"", []string{"a.conf", "b.conf"}, "", true},
	}
	for _, tc := range cases {
		got, err := resolveConfigPath(tc.flag, tc.args)
		if (err != nil) != tc.wantErr {
			t.Errorf("resolveConfigPath(%q, %v): err = %v, wantErr %v", tc.flag, tc.args, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveConfigPath(%q, %v) = %q, want %q", tc.flag, tc.args, got, tc.want)
		}
	}
}

// TestExampleConfigLoads pins the --example output to something Load accepts.
func TestExampleConfigLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "example.conf")
	if err := os.WriteFile(path, []byte(exampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatalf("the example config does not load: %v", err)
	}
	if cfg.Backend != "sqlite" || cfg.DBURI != "file:dbrest.db" {
		t.Errorf("example values not applied: backend=%q uri=%q", cfg.Backend, cfg.DBURI)
	}
}

// TestProbeReady exercises the --ready verb against a real admin server in
// both the ready and the not-ready state, plus the unconfigured error.
func TestProbeReady(t *testing.T) {
	cfgFor := func(t *testing.T, admin *adminapi.Server) *config.Config {
		t.Helper()
		ts := httptest.NewServer(admin)
		t.Cleanup(ts.Close)
		u, err := url.Parse(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.FromMap(map[string]string{"db-uri": "x"})
		if err != nil {
			t.Fatal(err)
		}
		cfg.AdminServerHost = u.Hostname()
		cfg.AdminServerPort = port
		return cfg
	}

	ready := cfgFor(t, &adminapi.Server{})
	if err := probeReady(ready); err != nil {
		t.Errorf("ready instance: %v", err)
	}

	pending := cfgFor(t, &adminapi.Server{
		Ready: func(context.Context) error { return errors.New("pending") },
	})
	if err := probeReady(pending); err == nil {
		t.Error("pending instance: expected an error")
	}

	ready.AdminServerPort = 0
	if err := probeReady(ready); err == nil {
		t.Error("no admin port: expected an error")
	}
}
