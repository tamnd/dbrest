package conformance_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/conformance"
	"github.com/tamnd/dbrest/httpapi"
)

// fixtureServer builds the films fixture on the SQLite backend with an FTS5
// index over the title, so the full-text probe has a covering index. It returns
// the server (the subject) and its backend's capabilities.
func fixtureServer(t *testing.T) (*httpapi.Server, sqliteCaps) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })

	_, err = be.DB().Exec(`
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
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")
	return srv, sqliteCaps{be}
}

// sqliteCaps wraps the backend so a test can read its declared capabilities.
type sqliteCaps struct{ be *sqlite.Backend }

func TestReplayGoldenPass(t *testing.T) {
	srv, _ := fixtureServer(t)
	cases := []conformance.Case{
		{
			Name:    "single row by id",
			Request: conformance.Request{Method: http.MethodGet, Path: "/films", Query: "id=eq.1"},
			Golden: conformance.Response{
				Status: 200,
				Body:   `[{"id":1,"title":"Metropolis","year":1927,"rating":"NR"}]`,
			},
		},
		{
			Name:    "ordered projection",
			Request: conformance.Request{Method: http.MethodGet, Path: "/films", Query: "select=id,title&order=id.asc"},
			Golden: conformance.Response{
				Status: 200,
				Body:   `[{"id":1,"title":"Metropolis"},{"id":2,"title":"Blade Runner"},{"id":3,"title":"Arrival"}]`,
			},
		},
	}
	rep := conformance.Replay(srv, cases, nil)
	if !rep.OK() {
		t.Fatalf("expected all cases to pass, report: %+v", rep.Results)
	}
	if rep.Passed != 2 {
		t.Errorf("passed = %d, want 2", rep.Passed)
	}
}

func TestReplayDetectsRegression(t *testing.T) {
	srv, _ := fixtureServer(t)
	// A golden the subject cannot reproduce (wrong title) must fail.
	cases := []conformance.Case{{
		Name:    "wrong golden",
		Request: conformance.Request{Method: http.MethodGet, Path: "/films", Query: "id=eq.1"},
		Golden:  conformance.Response{Status: 200, Body: `[{"id":1,"title":"WRONG","year":1927,"rating":"NR"}]`},
	}}
	rep := conformance.Replay(srv, cases, nil)
	if rep.OK() {
		t.Fatal("expected the regression to fail the run")
	}
	if rep.Results[0].Verdict != conformance.Fail {
		t.Errorf("verdict = %q, want fail", rep.Results[0].Verdict)
	}
}

func TestReplayUnsupportedIsPGRST127(t *testing.T) {
	srv, _ := fixtureServer(t)
	// An array-contains filter is Unsupported on SQLite; the golden records the
	// PGRST127 envelope, and the subject must reproduce it.
	cases := []conformance.Case{{
		Name:    "array contains is unsupported",
		Feature: "array-contains",
		Request: conformance.Request{Method: http.MethodGet, Path: "/films", Query: "title=cs.{a}"},
		Golden:  conformance.Response{Status: 400, Body: `{"code":"PGRST127","message":"","details":"","hint":""}`},
		Mask:    []string{"/message", "/details", "/hint"},
	}}
	al := conformance.NewAllowlist("sqlite", conformance.AllowEntry{
		Feature: "array-contains", Backend: "sqlite", Tier: "U",
		Request: "title=cs.{a}", Expected: "PGRST127", Reason: "no array types on SQLite",
	})
	rep := conformance.Replay(srv, cases, al)
	if !rep.OK() {
		t.Fatalf("expected the PGRST127 case to pass, got %+v", rep.Results)
	}
	if rep.Results[0].Verdict != conformance.Allowlisted {
		t.Errorf("verdict = %q, want allowlisted", rep.Results[0].Verdict)
	}
}

func TestCheckedInCorpus(t *testing.T) {
	srv, c := fixtureServer(t)
	cases, err := conformance.LoadCorpus("testdata/sqlite/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	al, err := conformance.LoadAllowlist("testdata/sqlite/allowlist.json")
	if err != nil {
		t.Fatal(err)
	}
	// The allowlist must reconcile with the live matrix before the run.
	features := map[string]backend.Tier{
		"fts":            backend.Native,
		"array-contains": c.be.Capabilities().ArrayRangeTypes,
	}
	if err := al.CheckMatrix(features); err != nil {
		t.Fatalf("allowlist disagrees with the matrix: %v", err)
	}
	rep := conformance.Replay(srv, cases, al)
	if !rep.OK() {
		for _, r := range rep.Results {
			if r.Verdict == conformance.Fail {
				t.Errorf("case %q failed: %v", r.Name, r.Diffs)
			}
		}
	}
}

func TestCapabilitySelfConsistency(t *testing.T) {
	srv, c := fixtureServer(t)
	caps := c.be.Capabilities()
	results := conformance.CheckCapabilities(srv, caps, conformance.DefaultProbes())
	if !conformance.CapabilitiesConsistent(results) {
		for _, r := range results {
			t.Logf("feature=%s tier=%s got127=%v consistent=%v", r.Feature, r.Tier, r.GotPGRST127, r.Consistent)
		}
		t.Fatal("declared capabilities disagree with observed behavior")
	}
}
