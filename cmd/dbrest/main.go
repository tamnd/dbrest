// Command dbrest serves a PostgREST-compatible REST API over a database. It
// reads its configuration from a file and the environment (spec 20), selects a
// backend, introspects the schema, and serves the HTTP frontend.
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/dbrest/adminapi"
	"github.com/tamnd/dbrest/auth"
	"github.com/tamnd/dbrest/authz"
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
		fmt.Println(versionLine())
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
	a.watchDBChannel(context.Background())

	if cfg.AdminEnabled() {
		startAdmin(cfg, be, a, metrics)
	}

	ln, err := listenAPI(cfg)
	if err != nil {
		where := cfg.ServerAddr()
		if cfg.ServerUnixSocket != "" {
			where = cfg.ServerUnixSocket
		}
		return fmt.Errorf("listen on %s: %w", where, err)
	}
	log.Printf("dbrest listening on %s (backend %s, %d relations)", ln.Addr(), cfg.Backend, a.Model().Len())
	logged := &requestLogger{next: a, level: a.logLevel, out: log.Default()}
	if err := http.Serve(ln, logged); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// listenAPI binds the API listener. With server-unix-socket set the socket
// replaces TCP entirely, the upstream behavior: a stale socket file from a
// previous run is removed, the socket is bound, and server-unix-socket-mode
// (already validated at load) is applied to it.
func listenAPI(cfg *config.Config) (net.Listener, error) {
	if cfg.ServerUnixSocket == "" {
		return listenFirst(cfg.Listeners())
	}
	if err := os.Remove(cfg.ServerUnixSocket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", cfg.ServerUnixSocket)
	if err != nil {
		return nil, err
	}
	mode, err := strconv.ParseUint(cfg.ServerUnixSocketMode, 8, 32)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("server-unix-socket-mode: %w", err)
	}
	if err := os.Chmod(cfg.ServerUnixSocket, os.FileMode(mode)); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// listenFirst binds the first candidate that works, in the preference order
// the host option encodes (the *4/*6 fallback story).
func listenFirst(specs []config.ListenSpec) (net.Listener, error) {
	var firstErr error
	for _, s := range specs {
		ln, err := net.Listen(s.Network, s.Addr)
		if err == nil {
			return ln, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
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
		ln, err := listenFirst(cfg.AdminListeners())
		if err != nil {
			log.Printf("dbrest: admin server: listen on %s: %v", cfg.AdminAddr(), err)
			return
		}
		log.Printf("dbrest admin listening on %s", ln.Addr())
		if err := http.Serve(ln, admin); err != nil {
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
	prepared := cfg.DBPreparedStatements
	be, err := backend.OpenWith(cfg.Backend, cfg.DBURI, backend.OpenOptions{PreparedStatements: &prepared})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	applyPoolConfig(be, cfg)
	applySchemaConfig(be, cfg)
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

// attachAuthz wires the emulated authorization layer from the policy-registry
// option. With no registry configured the gate stays off, which mirrors a
// database where every role holds every privilege; declaring a registry flips
// the model, and from then on the absence of a grant is a denial. A registry
// the parser cannot fully understand is a startup error, never a silently
// thinner rule set. Postgres delegates privileges and RLS to the engine, so a
// registry configured there is a misconfiguration and is refused too.
func attachAuthz(srv *httpapi.Server, cfg *config.Config) error {
	if cfg.PolicyRegistry == "" {
		return nil
	}
	if cfg.Backend == "postgres" {
		return fmt.Errorf("policy-registry: the postgres backend enforces grants and RLS natively; manage them in the database and unset the option")
	}
	reg, err := authz.ParseRegistry(cfg.PolicyRegistry)
	if err != nil {
		return err
	}
	srv.SetAuthz(reg)
	return nil
}

// applySchemaConfig pushes the schema-shaped options onto a backend that
// accepts them: the exposed schemas and db-extra-search-path, which extends
// type and function resolution without exposing the schemas. It runs at open
// and again on a config reload. Backends that have no schema notion ignore
// both by not implementing the setters.
func applySchemaConfig(be any, cfg *config.Config) {
	if sc, ok := be.(interface{ SetSchemas([]string) }); ok {
		sc.SetSchemas(cfg.Schemas)
	}
	if sp, ok := be.(interface{ SetExtraSearchPath([]string) }); ok {
		sp.SetExtraSearchPath(cfg.ExtraSearchPath)
	}
	if h, ok := be.(interface{ SetHoistedTxSettings([]string) }); ok {
		h.SetHoistedTxSettings(cfg.HoistedTxSettings)
	}
}

// applyPoolConfig sizes the connection pool on the engines built over
// database/sql (mysql, sqlserver). SQLite is left alone: its backend pins the
// pool to one connection so the foreign-key PRAGMA stays in effect, and
// resizing or recycling that connection would silently drop FK enforcement.
// The pgx-based postgres backend builds its pool inside Open and the
// acquisition timeout has no database/sql knob; both stay with the backend
// drivers as the per-driver remainder of the pool item.
func applyPoolConfig(be backend.Backend, cfg *config.Config) {
	if cfg.Backend == config.BackendSQLite {
		return
	}
	d, ok := be.(interface{ DB() *sql.DB })
	if !ok {
		return
	}
	db := d.DB()
	db.SetMaxOpenConns(cfg.DBPool)
	db.SetConnMaxIdleTime(cfg.DBPoolMaxIdleTime)
	db.SetConnMaxLifetime(cfg.DBPoolMaxLifetime)
}

// jwtSecretBytes returns the key material configured in jwt-secret. With
// jwt-secret-is-base64 set, the value is URL-safe base64 (padding optional)
// and an undecodable value is a boot error; silently keying the verifier with
// the wrong bytes would lock every valid token out.
func jwtSecretBytes(cfg *config.Config) ([]byte, error) {
	if !cfg.JWTSecretIsBase64 {
		return []byte(cfg.JWTSecret), nil
	}
	trimmed := strings.TrimRight(strings.TrimSpace(cfg.JWTSecret), "=")
	b, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("jwt-secret-is-base64 is set but jwt-secret is not valid URL-safe base64: %w", err)
	}
	return b, nil
}

// attachAuth wires a JWT verifier onto the server. The verifier is always
// attached so the server fails closed the way PostgREST does: with no key
// material a presented token is a 500 PGRST300, and with no anon role a
// tokenless request is a 401 PGRST302. The jwt-secret value is read the
// PostgREST way (JWK Set, JWK, or text secret), base64-decoded first when
// jwt-secret-is-base64 is set, and an unusable key configuration is a startup
// error.
func attachAuth(srv *httpapi.Server, cfg *config.Config) error {
	secret, err := jwtSecretBytes(cfg)
	if err != nil {
		return err
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
