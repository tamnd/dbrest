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
	"strconv"
	"time"

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
	if err := attachPreRequest(srv, be, cfg); err != nil {
		return err
	}

	log.Printf("dbrest listening on %s (backend %s, %d relations)", cfg.ServerAddr(), cfg.Backend, model.Len())
	if err := http.ListenAndServe(cfg.ServerAddr(), srv); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
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

// attachPreRequest wires the db-pre-request option. The function name rides the
// request context so the backend can invoke it after the session settings and
// before the main statement (spec 13). A backend that cannot honor it must not
// silently drop the option, since deployments use db-pre-request for blocking
// and custom auth; with no backend support declared, startup is refused.
func attachPreRequest(srv *httpapi.Server, be backend.Backend, cfg *config.Config) error {
	if cfg.PreRequest == "" {
		return nil
	}
	if pr, ok := be.(interface{ SupportsPreRequest() bool }); ok && pr.SupportsPreRequest() {
		srv.SetPreRequest(cfg.PreRequest)
		return nil
	}
	return fmt.Errorf("db-pre-request: the %s backend cannot run a pre-request function; unset the option", cfg.Backend)
}

// attachAuth wires a JWT verifier onto the server. The verifier is always
// attached so the server fails closed the way PostgREST does: with no key
// material a presented token is a 500 PGRST300, and with no anon role a
// tokenless request is a 401 PGRST302. The jwt-secret value is read the
// PostgREST way (JWK Set, JWK, or text secret), optionally base64-decoded
// first, and an unusable key configuration is a startup error.
func attachAuth(srv *httpapi.Server, cfg *config.Config) error {
	secret := []byte(cfg.JWTSecret)
	if v := os.Getenv("PGRST_JWT_SECRET_IS_BASE64"); v != "" {
		isB64, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("jwt-secret-is-base64: %w", err)
		}
		if isB64 && cfg.JWTSecret != "" {
			decoded, err := auth.DecodeBase64Secret(cfg.JWTSecret)
			if err != nil {
				return fmt.Errorf("jwt-secret-is-base64: %w", err)
			}
			secret = decoded
		}
	}
	v, err := auth.NewVerifier(auth.Config{
		Secret:          secret,
		JWKSet:          cfg.JWKSet,
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
