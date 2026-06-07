// Command dbrest serves a PostgREST-compatible REST API over a database. This
// entry point wires the SQLite reference backend to the HTTP frontend; the
// configuration subsystem (spec 20) replaces the flags with a full config file
// and multi-backend selection as it lands.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/tamnd/dbrest/backend/sqlite"
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
	var (
		addr = flag.String("addr", ":3000", "listen address")
		dsn  = flag.String("db", "dbrest.sqlite", "SQLite database path or DSN")
	)
	flag.Parse()

	be, err := sqlite.Open(*dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = be.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	model, err := be.Introspect(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("introspect: %w", err)
	}

	srv := httpapi.NewServer(be, model, []string{""})

	log.Printf("dbrest listening on %s (db %s, %d relations)", *addr, *dsn, model.Len())
	if err := http.ListenAndServe(*addr, srv); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
