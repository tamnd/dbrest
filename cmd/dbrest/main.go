// Command dbrest serves a PostgREST-compatible REST API over a database. It
// reads its configuration from a file and the environment (spec 20), selects a
// backend, introspects the schema, and serves the HTTP frontend.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/tamnd/dbrest/auth"
	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/postgres"
	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/config"
	"github.com/tamnd/dbrest/httpapi"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("dbrest: %v", err)
	}
}

// run holds the real entry point so deferred cleanup (closing the backend) runs
// on every exit path; main only translates a returned error into a fatal log.
func run() error {
	var configPath string
	flag.StringVar(&configPath, "config", "", "path to the configuration file (env-only if omitted)")
	flag.Parse()

	cfg, err := config.Load(configPath, os.Environ())
	if err != nil {
		return err
	}

	be, err := openBackend(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = be.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	model, err := be.Introspect(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("introspect: %w", err)
	}

	srv := httpapi.NewServer(be, model, cfg.Schemas)
	srv.SetDefaultRole(cfg.AnonRole)
	srv.SetOpenAPI(cfg.OpenAPIMode, cfg.OpenAPIServerProxyURI)
	if err := attachAuth(srv, cfg); err != nil {
		return err
	}

	log.Printf("dbrest listening on %s (backend %s, %d relations)", cfg.ServerAddr(), cfg.Backend, model.Len())
	if err := http.ListenAndServe(cfg.ServerAddr(), srv); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// openBackend opens the engine the configuration selected.
func openBackend(cfg *config.Config) (backend.Backend, error) {
	switch cfg.Backend {
	case config.BackendSQLite:
		be, err := sqlite.Open(cfg.DBURI)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		return be, nil
	case config.BackendPostgres:
		be, err := postgres.Open(cfg.DBURI)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		be.SetSchemas(cfg.Schemas)
		return be, nil
	case config.BackendMySQL, config.BackendSQLServer:
		// The MySQL/MariaDB and SQL Server dialects have landed but the live driver
		// data plane is a separate slice.
		return nil, fmt.Errorf("db-backend %q has its dialect but no runnable data plane yet", cfg.Backend)
	case config.BackendMongoDB:
		// The MongoDB query lowering has landed but the live driver data plane is a
		// separate slice.
		return nil, fmt.Errorf("db-backend %q has its query lowering but no runnable data plane yet", cfg.Backend)
	default:
		return nil, fmt.Errorf("db-backend %q is unknown", cfg.Backend)
	}
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
