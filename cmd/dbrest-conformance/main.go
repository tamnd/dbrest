// Command dbrest-conformance runs a conformance pass against one backend: it
// builds the fixture, starts an in-process dbrest server, replays the request
// corpus, compares each response to its recorded golden under the allowlist, and
// runs the capability self-consistency check. It is the local reproduction of
// what the CI matrix does per backend (spec 22 section 10).
//
// Only the SQLite backend is wired today, with the films fixture; another
// backend joins by adding its fixture and capabilities here once its driver
// lands.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/tamnd/dbrest/backend"
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
		backendName = flag.String("backend", "sqlite", "backend to run the conformance pass against")
		corpusPath  = flag.String("corpus", "conformance/testdata/sqlite/corpus.json", "request corpus file")
		allowPath   = flag.String("allowlist", "conformance/testdata/sqlite/allowlist.json", "allowlist file")
	)
	flag.Parse()

	if *backendName != "sqlite" {
		return fmt.Errorf("backend %q is not wired into the harness yet; only sqlite is available", *backendName)
	}

	srv, be, err := sqliteFixture()
	if err != nil {
		return err
	}
	defer func() { _ = be.Close() }()
	caps := be.Capabilities()

	cases, err := conformance.LoadCorpus(*corpusPath)
	if err != nil {
		return err
	}
	allow, err := conformance.LoadAllowlist(*allowPath)
	if err != nil {
		return err
	}
	if err := allow.CheckMatrix(featureTiers(caps)); err != nil {
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
	return httpapi.NewServer(be, model, nil), be, nil
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
