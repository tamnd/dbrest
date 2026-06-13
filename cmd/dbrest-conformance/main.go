// Command dbrest-conformance runs a conformance pass against one backend: it
// builds the fixture, starts an in-process dbrest server, replays the request
// corpus, compares each response to its recorded golden under the allowlist, and
// runs the capability self-consistency check. It is the local reproduction of
// what the CI matrix does per backend (spec 22 section 10).
//
// The SQLite and PostgreSQL backends are wired, each with a films fixture;
// another backend joins by adding its fixture and capabilities here once its
// driver lands. The postgres pass needs a live server, read from DBREST_PG_DSN
// or the -dsn flag, and it is the reference backend, so its corpus golden is the
// upstream PostgreSQL output and its allowlist documents no divergence.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/postgres"
	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/conformance"
	"github.com/tamnd/dbrest/httpapi"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("dbrest-conformance: %v", err)
	}
}

func run() error {
	var (
		backendName = flag.String("backend", "sqlite", "backend to run the conformance pass against (sqlite or postgres)")
		corpusPath  = flag.String("corpus", "", "request corpus file (defaults to the backend's testdata corpus)")
		allowPath   = flag.String("allowlist", "", "allowlist file (defaults to the backend's testdata allowlist)")
		dsn         = flag.String("dsn", "", "postgres DSN; falls back to DBREST_PG_DSN")
	)
	flag.Parse()

	if *corpusPath == "" {
		*corpusPath = fmt.Sprintf("conformance/testdata/%s/corpus.json", *backendName)
	}
	if *allowPath == "" {
		*allowPath = fmt.Sprintf("conformance/testdata/%s/allowlist.json", *backendName)
	}

	var (
		srv     *httpapi.Server
		caps    backend.Capabilities
		closeBE func()
		tiers   map[string]backend.Tier
		err     error
	)
	switch *backendName {
	case "sqlite":
		var be *sqlite.Backend
		srv, be, err = sqliteFixture()
		if err != nil {
			return err
		}
		closeBE = func() { _ = be.Close() }
		caps = be.Capabilities()
		tiers = featureTiers(caps)
	case "postgres":
		conn := *dsn
		if conn == "" {
			conn = os.Getenv("DBREST_PG_DSN")
		}
		if conn == "" {
			return fmt.Errorf("postgres backend needs a DSN: pass -dsn or set DBREST_PG_DSN")
		}
		var be *postgres.Backend
		srv, be, err = postgresFixture(conn)
		if err != nil {
			return err
		}
		closeBE = func() { _ = be.Close() }
		caps = be.Capabilities()
		tiers = featureTiers(caps)
	default:
		return fmt.Errorf("backend %q is not wired into the harness; available: sqlite, postgres", *backendName)
	}
	defer closeBE()

	cases, err := conformance.LoadCorpus(*corpusPath)
	if err != nil {
		return err
	}
	allow, err := conformance.LoadAllowlist(*allowPath)
	if err != nil {
		return err
	}
	if err := allow.CheckMatrix(tiers); err != nil {
		return err
	}

	rep := conformance.Replay(srv, cases, allow)
	caps2 := conformance.CheckCapabilities(srv, caps, conformance.DefaultProbes())

	printReport(*backendName, rep, caps2)
	if !rep.OK() || !conformance.CapabilitiesConsistent(caps2) {
		return fmt.Errorf("conformance failed: %d cases failed", rep.Failed)
	}
	return nil
}

// sqliteFixture builds the films fixture (with an FTS5 index over the title) on
// an in-memory SQLite backend and returns a server over it.
func sqliteFixture() (*httpapi.Server, *sqlite.Backend, error) {
	be, err := sqlite.Open("file:conformance?mode=memory&cache=shared")
	if err != nil {
		return nil, nil, fmt.Errorf("open: %w", err)
	}
	if _, err := be.DB().Exec(fixtureDDL); err != nil {
		return nil, nil, fmt.Errorf("load fixture: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	model, err := be.Introspect(ctx)
	cancel()
	if err != nil {
		return nil, nil, fmt.Errorf("introspect: %w", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")
	return srv, be, nil
}

const fixtureDDL = `
CREATE TABLE films (
	id     INTEGER PRIMARY KEY,
	title  TEXT NOT NULL,
	year   INTEGER,
	rating TEXT
);
INSERT INTO films (id, title, year, rating) VALUES
	(1, 'Metropolis', 1927, 'NR'),
	(2, 'Blade Runner', 1982, 'R'),
	(3, 'Arrival', 2016, 'PG13');
CREATE VIRTUAL TABLE films_fts USING fts5(title, content='films', content_rowid='id');
INSERT INTO films_fts (rowid, title) SELECT id, title FROM films;
`

// postgresFixture builds the films fixture on a live PostgreSQL server in a
// dedicated schema and returns a server over it. Postgres is the reference
// backend: its corpus golden is the upstream output, so the fixture mirrors the
// sqlite one plus a tags array column, since arrays are Native here where SQLite
// has none. The anon role is created so the server's SET LOCAL ROLE has an
// identity to assume, matching the role-emulation path a real deployment uses.
func postgresFixture(dsn string) (*httpapi.Server, *postgres.Backend, error) {
	be, err := postgres.Open(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := be.Pool().Exec(ctx, pgFixtureDDL); err != nil {
		_ = be.Close()
		return nil, nil, fmt.Errorf("load fixture: %w", err)
	}
	be.SetSchemas([]string{"_dbrest_conf"})
	model, err := be.Introspect(ctx)
	if err != nil {
		_ = be.Close()
		return nil, nil, fmt.Errorf("introspect: %w", err)
	}
	srv := httpapi.NewServer(be, model, []string{"_dbrest_conf"})
	srv.SetDefaultRole("anon")
	return srv, be, nil
}

const pgFixtureDDL = `
DROP SCHEMA IF EXISTS _dbrest_conf CASCADE;
CREATE SCHEMA _dbrest_conf;
DO $$ BEGIN
	IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'anon') THEN
		CREATE ROLE anon NOLOGIN;
	END IF;
END $$;
GRANT USAGE ON SCHEMA _dbrest_conf TO anon;
CREATE TABLE _dbrest_conf.films (
	id     integer PRIMARY KEY,
	title  text NOT NULL,
	year   integer,
	rating text,
	tags   text[]
);
INSERT INTO _dbrest_conf.films (id, title, year, rating, tags) VALUES
	(1, 'Metropolis', 1927, 'NR', '{sci-fi,silent}'),
	(2, 'Blade Runner', 1982, 'R', '{sci-fi,noir}'),
	(3, 'Arrival', 2016, 'PG13', '{sci-fi,drama}');
GRANT SELECT ON _dbrest_conf.films TO anon;
`

// featureTiers maps the allowlist's feature labels to the tier each resolves to
// on this backend, so the allowlist can be reconciled with the live matrix.
func featureTiers(caps backend.Capabilities) map[string]backend.Tier {
	ft := backend.Native
	if caps.FullText == backend.FTNone {
		ft = backend.Unsupported
	}
	return map[string]backend.Tier{
		"regex":          caps.Regex,
		"fts":            ft,
		"array-contains": caps.ArrayRangeTypes,
		"count-planned":  caps.CountPlanned,
	}
}

func printReport(backendName string, rep conformance.Report, caps []conformance.CapResult) {
	fmt.Printf("conformance: backend %s\n", backendName)
	for _, r := range rep.Results {
		fmt.Printf("  [%-11s] %s\n", r.Verdict, r.Name)
		for _, d := range r.Diffs {
			fmt.Printf("      %s\n", d)
		}
	}
	fmt.Printf("  replay: %d passed, %d allowlisted, %d failed\n", rep.Passed, rep.Allowed, rep.Failed)
	for _, c := range caps {
		status := "ok"
		if !c.Consistent {
			status = "INCONSISTENT"
		}
		fmt.Printf("  capability %-15s tier=%s pgrst127=%v %s\n", c.Feature, c.Tier, c.GotPGRST127, status)
	}
}
