package conformance_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/postgres"
	"github.com/tamnd/dbrest/conformance"
	"github.com/tamnd/dbrest/httpapi"
)

// pgFixtureDDL mirrors cmd/dbrest-conformance: the films fixture in a dedicated
// schema, plus a text[] column so the array operators exercise the Native tier
// that SQLite lacks, and the anon role the server's SET LOCAL ROLE assumes.
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

// TestPostgresConformanceCorpus replays the checked-in postgres corpus against a
// live PostgreSQL fixture under the postgres allowlist, and reconciles the
// allowlist against the live capability matrix. It is the in-process twin of the
// dbrest-conformance CLI's postgres pass, gated on DBREST_PG_DSN so the suite
// stays green without a server. Postgres is the reference backend: every case
// passes natively and the allowlist documents no divergence.
func TestPostgresConformanceCorpus(t *testing.T) {
	dsn := os.Getenv("DBREST_PG_DSN")
	if dsn == "" {
		t.Skip("DBREST_PG_DSN not set; skipping postgres conformance corpus")
	}

	be, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := be.Pool().Exec(ctx, pgFixtureDDL); err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = be.Pool().Exec(context.Background(), "DROP SCHEMA IF EXISTS _dbrest_conf CASCADE")
	})

	be.SetSchemas([]string{"_dbrest_conf"})
	model, err := be.Introspect(ctx)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, []string{"_dbrest_conf"})
	srv.SetDefaultRole("anon")

	cases, err := conformance.LoadCorpus("testdata/postgres/corpus.json")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	allow, err := conformance.LoadAllowlist("testdata/postgres/allowlist.json")
	if err != nil {
		t.Fatalf("load allowlist: %v", err)
	}

	caps := be.Capabilities()
	ft := backend.Native
	if caps.FullText == backend.FTNone {
		ft = backend.Unsupported
	}
	tiers := map[string]backend.Tier{
		"regex":          caps.Regex,
		"fts":            ft,
		"array-contains": caps.ArrayRangeTypes,
		"count-planned":  caps.CountPlanned,
	}
	if err := allow.CheckMatrix(tiers); err != nil {
		t.Fatalf("allowlist vs matrix: %v", err)
	}

	rep := conformance.Replay(srv, cases, allow)
	if !rep.OK() {
		for _, r := range rep.Results {
			if r.Verdict == conformance.Fail {
				t.Errorf("case %q: %s %v", r.Name, r.Verdict, r.Diffs)
			}
		}
		t.Fatalf("postgres corpus: %d passed, %d allowlisted, %d failed", rep.Passed, rep.Allowed, rep.Failed)
	}
	if rep.Failed != 0 || rep.Allowed != 0 {
		t.Errorf("reference backend should pass every case natively: %d failed, %d allowlisted", rep.Failed, rep.Allowed)
	}
}
