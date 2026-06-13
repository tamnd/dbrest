package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
)

// newFTSServer mirrors newServer but adds an external-content FTS5 index over the
// films title so an fts filter has a covering full-text index to match against.
func newFTSServer(t testing.TB) *httpapi.Server {
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
			(3, 'Arrival', 2016, 'PG13'),
			(4, 'Untitled', NULL, 'NR');
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
	return srv
}

// TestFTSMatchSelectsRow exercises the full request path: an fts filter lowers to
// an FTS5 MATCH and returns only the indexed row.
func TestFTSMatchSelectsRow(t *testing.T) {
	srv := newFTSServer(t)
	resp := do(t, srv, http.MethodGet, "/films?title=fts.metropolis", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Metropolis" {
		t.Fatalf("rows = %v, want one Metropolis", rows)
	}
}

// TestFTSMissingIndexIs400 checks that an fts filter on a column without a covering
// FTS5 index is the PGRST127 envelope rather than a silent substring scan.
func TestFTSMissingIndexIs400(t *testing.T) {
	srv := newServer(t) // the shared films table has no FTS5 index
	resp := do(t, srv, http.MethodGet, "/films?title=fts.metropolis", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST127" {
		t.Errorf("code = %v, want PGRST127", env["code"])
	}
}

// TestRegexMatchSelectsRows checks a case-sensitive regex filter against the live
// data; modernc's registered regexp() compiles the RE2 pattern.
func TestRegexMatchSelectsRows(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?title=match.^Bl", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Blade Runner" {
		t.Fatalf("rows = %v, want one Blade Runner", rows)
	}
}

// TestRegexIMatchIsCaseInsensitive checks imatch ignores case.
func TestRegexIMatchIsCaseInsensitive(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?title=imatch.arrival", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 1 || rows[0]["title"] != "Arrival" {
		t.Fatalf("rows = %v, want one Arrival", rows)
	}
}

// TestRegexBackreferenceIs400 checks an RE2-incompatible backreference pattern is
// rejected before lowering with PGRST127, not failed deep in the engine.
func TestRegexBackreferenceIs400(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, `/films?title=match.(a)\1`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST127" {
		t.Errorf("code = %v, want PGRST127", env["code"])
	}
}
