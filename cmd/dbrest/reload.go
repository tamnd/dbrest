// Reload plumbing: PostgREST re-reads its schema cache on SIGUSR1 and its
// configuration on SIGUSR2, without dropping the listener. dbrest does the
// same by keeping the HTTP frontend behind an atomic handler and rebuilding
// it from the new inputs; an in-flight request keeps the snapshot it started
// with. A failed reload logs and keeps serving with the previous state, the
// upstream behavior. The per-driver paths (LISTEN on db-channel, db-config,
// db-pre-config) live with each backend and are not wired here yet.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
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
	srv.SetPlanEnabled(a.cfg.PlanEnabled)
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

// watchSignals installs the two reload signals. Reload failures log and keep
// the previous state; they never terminate the process.
func (a *app) watchSignals() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1, syscall.SIGUSR2)
	go func() {
		for s := range ch {
			switch s {
			case syscall.SIGUSR1:
				log.Printf("dbrest: received SIGUSR1, reloading the schema cache")
				if err := a.reloadSchema(); err != nil {
					log.Printf("dbrest: schema cache reload failed, keeping the old cache: %v", err)
				}
			case syscall.SIGUSR2:
				log.Printf("dbrest: received SIGUSR2, reloading the configuration")
				if err := a.reloadConfig(os.Environ()); err != nil {
					log.Printf("dbrest: config reload failed, keeping the old config: %v", err)
				}
			}
		}
	}()
}
