// Reload plumbing: PostgREST re-reads its schema cache on SIGUSR1 and its
// configuration on SIGUSR2, without dropping the listener. dbrest does the
// same by keeping the HTTP frontend behind an atomic handler and rebuilding
// it from the new inputs; an in-flight request keeps the snapshot it started
// with. A failed reload logs and keeps serving with the previous state, the
// upstream behavior. The signal handler is platform-specific and lives in
// reload_signals_unix.go (the Unix signals do not exist on Windows); the
// db-channel listener lives in watchDBChannel below.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/dbrest/adminapi"
	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/config"
	"github.com/tamnd/dbrest/httpapi"
	"github.com/tamnd/dbrest/schema"
)

// app owns the pieces a reload swaps: the configuration, the schema cache,
// and the frontend built from them.
type app struct {
	cfgPath string
	be      backend.Backend
	metrics *adminapi.Metrics

	mu    sync.Mutex // serializes reloads; guards cfg and model
	cfg   *config.Config
	model *schema.Model

	handler atomic.Value // always a *httpapi.Server
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.handler.Load().(http.Handler).ServeHTTP(w, r)
}

// Model is the schema cache currently being served.
func (a *app) Model() *schema.Model {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// logLevel is the log-level currently in force; the request logger reads it
// per request so a config reload changes it live.
func (a *app) logLevel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.LogLevel
}

// reloadSchema re-introspects the database and swaps in a frontend built on
// the fresh cache. It is both the boot-time load and the SIGUSR1 handler; on
// failure the old cache stays in service.
func (a *app) reloadSchema() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	started := time.Now()
	model, err := a.be.Introspect(ctx)
	cancel()
	a.metrics.ObserveSchemaCacheLoad(time.Since(started), err)
	if err != nil {
		return err
	}
	a.model = model
	return a.rebuildLocked()
}

// reloadConfig re-reads the configuration sources and applies the reloadable
// subset, logging every boot-time option whose change had to be ignored. It
// is the SIGUSR2 handler; a config that does not load leaves the old one in
// service.
func (a *app) reloadConfig(environ []string) error {
	next, err := config.Load(a.cfgPath, environ)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	merged, kept := a.cfg.MergeReloadable(next)
	for _, msg := range kept {
		log.Printf("dbrest: reload: %s", msg)
	}
	for _, w := range merged.Warnings {
		log.Printf("dbrest: warning: %s", w)
	}
	a.cfg = merged
	applySchemaConfig(a.be, merged)
	return a.rebuildLocked()
}

// rebuildLocked builds the frontend from the current cfg and model and swaps
// it in. The caller holds a.mu.
func (a *app) rebuildLocked() error {
	srv := httpapi.NewServer(a.be, a.model, a.cfg.Schemas)
	srv.SetDefaultRole(a.cfg.AnonRole)
	srv.SetOpenAPI(a.cfg.OpenAPIMode, a.cfg.OpenAPIServerProxyURI, a.cfg.OpenAPISecurityActive)
	srv.SetRootSpec(a.cfg.RootSpec)
	srv.SetCORSAllowedOrigins(a.cfg.CORSAllowedOrigins)
	srv.SetMaxRows(a.cfg.MaxRows)
	srv.SetMaxRequestBody(a.cfg.MaxRequestBody)
	srv.SetServerTimingEnabled(a.cfg.ServerTimingEnabled)
	srv.SetTxEnd(a.cfg.TxEnd)
	srv.SetPlanEnabled(a.cfg.PlanEnabled)
	srv.SetAggregatesEnabled(a.cfg.AggregatesEnabled)
	srv.SetPreRequest(a.cfg.PreRequest)
	srv.SetAppSettings(a.cfg.AppSettings)
	srv.SetLogQuery(a.cfg.LogQuery)
	if err := attachAuth(srv, a.cfg); err != nil {
		return err
	}
	if err := attachPreRequest(srv, a.be, a.cfg); err != nil {
		return err
	}
	if err := attachAuthz(srv, a.cfg); err != nil {
		return err
	}
	a.handler.Store(srv)
	return nil
}

// reloadAction is what a db-channel notification asks the server to do.
type reloadAction int

const (
	reloadNone reloadAction = iota
	reloadActionSchema
	reloadActionConfig
)

// dbNotifyAction decodes a db-channel NOTIFY payload into a reload action,
// implementing PostgREST's contract: an empty payload or "reload schema" reloads
// the schema cache, "reload config" reloads the configuration, and any other
// payload is ignored.
func dbNotifyAction(payload string) reloadAction {
	switch payload {
	case "", "reload schema":
		return reloadActionSchema
	case "reload config":
		return reloadActionConfig
	default:
		return reloadNone
	}
}

// handleDBNotify applies the reload a db-channel payload asks for. Like the
// signal handlers, a failed reload logs and keeps the previous state.
func (a *app) handleDBNotify(payload string) {
	switch dbNotifyAction(payload) {
	case reloadActionSchema:
		log.Printf("dbrest: db-channel notification, reloading the schema cache")
		if err := a.reloadSchema(); err != nil {
			log.Printf("dbrest: schema cache reload failed, keeping the old cache: %v", err)
		}
	case reloadActionConfig:
		log.Printf("dbrest: db-channel notification, reloading the configuration")
		if err := a.reloadConfig(os.Environ()); err != nil {
			log.Printf("dbrest: config reload failed, keeping the old config: %v", err)
		}
	}
}

// watchDBChannel starts PostgREST's db-channel listener when db-channel-enabled
// is set and the backend can listen. A NOTIFY drives reloads through
// handleDBNotify; a reconnect refreshes the schema cache because notifications
// sent while the listener was down are lost. A backend with no LISTEN support is
// silently skipped, leaving signal-driven reloads in place.
func (a *app) watchDBChannel(ctx context.Context) {
	a.mu.Lock()
	enabled := a.cfg.DBChannelEnabled
	channel := a.cfg.DBChannel
	a.mu.Unlock()
	if !enabled {
		return
	}
	l, ok := a.be.(backend.Listener)
	if !ok {
		log.Printf("dbrest: backend does not support db-channel; reloads are signal-driven only")
		return
	}
	h := backend.ListenHandler{
		OnNotify: a.handleDBNotify,
		OnReconnect: func() {
			log.Printf("dbrest: db-channel %q reconnected, reloading the schema cache", channel)
			if err := a.reloadSchema(); err != nil {
				log.Printf("dbrest: schema cache reload failed, keeping the old cache: %v", err)
			}
		},
	}
	go func() {
		if err := l.Listen(ctx, channel, h); err != nil && ctx.Err() == nil {
			log.Printf("dbrest: db-channel listener stopped: %v", err)
		}
	}()
}
