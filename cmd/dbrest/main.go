// Command dbrest serves a PostgREST-compatible REST API over a database. It
// reads its configuration from a file and the environment (spec 20), selects a
// backend, introspects the schema, and serves the HTTP frontend.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/tamnd/dbrest/adminapi"
	"github.com/tamnd/dbrest/auth"
	"github.com/tamnd/dbrest/backend"
	_ "github.com/tamnd/dbrest/backend/mongo"
	_ "github.com/tamnd/dbrest/backend/mysql"
	_ "github.com/tamnd/dbrest/backend/postgres"
	_ "github.com/tamnd/dbrest/backend/sqlite"
	_ "github.com/tamnd/dbrest/backend/sqlserver"
	"github.com/tamnd/dbrest/config"
	"github.com/tamnd/dbrest/httpapi"
	"github.com/tamnd/dbrest/rpc"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("dbrest: %v", err)
	}
}

// run holds the real entry point so deferred cleanup (closing the backend) runs
// on every exit path; main only translates a returned error into a fatal log.
func run() error {
	var (
		configPath  string
		showVersion bool
		example     bool
		dumpConfig  bool
		dumpSchema  bool
		ready       bool
	)
	flag.StringVar(&configPath, "config", "", "path to the configuration file (env-only if omitted)")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.BoolVar(&showVersion, "v", false, "print the version and exit (shorthand)")
	flag.BoolVar(&example, "example", false, "print an example configuration file and exit")
	flag.BoolVar(&example, "e", false, "print an example configuration file and exit (shorthand)")
	flag.BoolVar(&dumpConfig, "dump-config", false, "print the resolved configuration and exit")
	flag.BoolVar(&dumpSchema, "dump-schema", false, "print the schema cache as JSON and exit")
	flag.BoolVar(&ready, "ready", false, "probe a running instance's admin /ready and exit 0 or 1")
	flag.Parse()

	configPath, err := resolveConfigPath(configPath, flag.Args())
	if err != nil {
		return err
	}
	if showVersion {
		fmt.Println("dbrest " + versionString())
		return nil
	}
	if example {
		fmt.Print(exampleConfig)
		return nil
	}

	cfg, err := config.Load(configPath, os.Environ())
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		log.Printf("dbrest: warning: %s", w)
	}

	if dumpConfig {
		fmt.Print(cfg.Dump())
		return nil
	}
	if ready {
		return probeReady(cfg)
	}

	be, err := openBackend(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = be.Close() }()

	if dumpSchema {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		model, err := be.Introspect(ctx)
		if err != nil {
			return fmt.Errorf("introspect: %w", err)
		}
		out, err := json.MarshalIndent(map[string]any{"relations": model.Relations()}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	metrics := adminapi.NewMetrics(cfg.DBPool)
	a := &app{cfgPath: configPath, be: be, cfg: cfg, metrics: metrics}
	if err := a.reloadSchema(); err != nil {
		return fmt.Errorf("introspect: %w", err)
	}
	a.watchSignals()

	if cfg.AdminEnabled() {
		startAdmin(cfg, be, a, metrics)
	}

	log.Printf("dbrest listening on %s (backend %s, %d relations)", cfg.ServerAddr(), cfg.Backend, a.Model().Len())
	if err := http.ListenAndServe(cfg.ServerAddr(), a); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// startAdmin runs the admin listener (admin-server-port) next to the API: the
// /live and /ready probes, the /schema_cache dump, and /metrics. The liveness
// check dials the API socket the way PostgREST's admin server does.
func startAdmin(cfg *config.Config, be backend.Backend, a *app, metrics *adminapi.Metrics) {
	apiAddr := probeAddr(cfg.ServerHost, cfg.ServerPort)
	admin := &adminapi.Server{
		Live: func(ctx context.Context) error {
			d := net.Dialer{Timeout: time.Second}
			conn, err := d.DialContext(ctx, "tcp", apiAddr)
			if err != nil {
				return err
			}
			return conn.Close()
		},
		Ready: func(ctx context.Context) error {
			// A backend that can check its connection exposes Ping; one that
			// cannot (an embedded engine) is ready once the cache is loaded.
			if p, ok := be.(interface{ Ping(context.Context) error }); ok {
				return p.Ping(ctx)
			}
			return nil
		},
		SchemaCache: func() ([]byte, error) {
			return json.Marshal(map[string]any{"relations": a.Model().Relations()})
		},
		Metrics: metrics,
	}
	go func() {
		log.Printf("dbrest admin listening on %s", cfg.AdminAddr())
		if err := http.ListenAndServe(cfg.AdminAddr(), admin); err != nil {
			log.Printf("dbrest: admin server: %v", err)
		}
	}()
}

// probeAddr is the address the liveness probe dials. A wildcard listen host is
// not dialable as written, so the probe goes through loopback.
func probeAddr(host string, port int) string {
	switch host {
	case "", "0.0.0.0", "*", "*4", "!4":
		host = "127.0.0.1"
	case "::", "*6", "!6":
		host = "::1"
	}
	return net.JoinHostPort(host, fmt.Sprint(port))
}

// openBackend opens the engine the configuration selected.
// Each backend driver self-registers via its package init function; this file
// imports them as blank imports so their init functions run.
func openBackend(cfg *config.Config) (backend.Backend, error) {
	be, err := backend.Open(cfg.Backend, cfg.DBURI)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if sc, ok := be.(interface{ SetSchemas([]string) }); ok {
		sc.SetSchemas(cfg.Schemas)
	}
	// Wire declared function registry for backends that cannot discover
	// functions from an engine catalog (NativeRPC=false: SQLite, MySQL, …).
	if cfg.FunctionRegistry != "" {
		reg, err := rpc.ParseRegistry(cfg.FunctionRegistry)
		if err != nil {
			return nil, fmt.Errorf("function-registry: %w", err)
		}
		if r, ok := be.(interface{ Register(rpc.Registry) }); ok {
			r.Register(reg)
		}
	}
	return be, nil
}

// attachAuth wires a JWT verifier onto the server when a key is configured.
// With no key material the server runs every request as the static anon role,
// which is the PostgREST behavior for an unconfigured jwt-secret.
func attachAuth(srv *httpapi.Server, cfg *config.Config) error {
	if cfg.JWTSecret == "" && cfg.JWKSet == "" {
		return nil
	}
	v, err := auth.NewVerifier(auth.Config{
		Secret:          []byte(cfg.JWTSecret),
		Audience:        cfg.JWTAud,
		RoleClaimKey:    cfg.JWTRoleClaimKey,
		AnonRole:        cfg.AnonRole,
		CacheMaxEntries: cfg.JWTCacheMaxEntries,
	})
	if err != nil {
		return fmt.Errorf("jwt: %w", err)
	}
	srv.SetVerifier(v)
	return nil
}
