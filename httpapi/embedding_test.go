package httpapi_test

import (
	"context"
	"encoding/csv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
)

// newEmbedServer seeds the canonical embedding fixture: directors own films
// (a forward FK from films, so films->director is to-one and directors->films is
// to-many), and films relate to actors through the roles junction (many-to-many).
// Film 4 has a NULL director to exercise the to-one null case.
func newEmbedServer(t testing.TB) *httpapi.Server {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	be, err := sqlite.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { be.Close() })

	_, err = be.DB().Exec(`
		CREATE TABLE directors (
			id   INTEGER PRIMARY KEY,
			name TEXT NOT NULL
		);
		CREATE TABLE films (
			id          INTEGER PRIMARY KEY,
			title       TEXT NOT NULL,
			year        INTEGER,
			director_id INTEGER REFERENCES directors(id)
		);
		CREATE TABLE actors (
			id   INTEGER PRIMARY KEY,
			name TEXT NOT NULL
		);
		CREATE TABLE roles (
			film_id  INTEGER NOT NULL REFERENCES films(id),
			actor_id INTEGER NOT NULL REFERENCES actors(id),
			PRIMARY KEY (film_id, actor_id)
		);
		INSERT INTO directors (id, name) VALUES
			(1, 'Lang'), (2, 'Scott'), (3, 'Villeneuve');
		INSERT INTO films (id, title, year, director_id) VALUES
			(1, 'Metropolis', 1927, 1),
			(2, 'Blade Runner', 1982, 2),
			(3, 'Arrival', 2016, 3),
			(4, 'Untitled', NULL, NULL);
		INSERT INTO actors (id, name) VALUES
			(1, 'Ford'), (2, 'Hauer'), (3, 'Adams');
		INSERT INTO roles (film_id, actor_id) VALUES
			(2, 1), (2, 2), (3, 3);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	return httpapi.NewServer(be, model, nil)
}

func TestEmbedToOneObject(t *testing.T) {
	srv := newEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title,director:directors(name)&order=id", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	rows := decodeArray(t, resp)
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	d, ok := rows[0]["director"].(map[string]any)
	if !ok {
		t.Fatalf("director = %#v, want an object", rows[0]["director"])
	}
	if d["name"] != "Lang" {
		t.Errorf("director.name = %v, want Lang", d["name"])
	}
	// The to-one embed projects only the named column.
	if _, has := d["id"]; has {
		t.Error("director should not carry id; only name was selected")
	}
}

func TestEmbedToOneNull(t *testing.T) {
	srv := newEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title,director:directors(name)&id=eq.4", nil)
	rows := decodeArray(t, resp)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0]["director"] != nil {
		t.Errorf("director = %#v, want null for a film with no director", rows[0]["director"])
	}
}

func TestEmbedToManyArray(t *testing.T) {
	srv := newEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/directors?select=name,films(title)&order=id", nil)
	rows := decodeArray(t, resp)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	films, ok := rows[0]["films"].([]any)
	if !ok {
		t.Fatalf("films = %#v, want an array", rows[0]["films"])
	}
	if len(films) != 1 {
		t.Fatalf("director 1 has %d films, want 1", len(films))
	}
	first := films[0].(map[string]any)
	if first["title"] != "Metropolis" {
		t.Errorf("film title = %v, want Metropolis", first["title"])
	}
}

func TestEmbedToManyEmptyArray(t *testing.T) {
	srv := newEmbedServer(t)
	// Director 1 (Lang) has exactly one film; insert no roles for that film, so a
	// films->actors embed on it yields an empty array, not null.
	resp := do(t, srv, http.MethodGet, "/films?select=title,actors(name)&id=eq.1", nil)
	rows := decodeArray(t, resp)
	actors, ok := rows[0]["actors"].([]any)
	if !ok {
		t.Fatalf("actors = %#v, want an array", rows[0]["actors"])
	}
	if len(actors) != 0 {
		t.Errorf("actors = %v, want an empty array", actors)
	}
}

func TestEmbedManyToMany(t *testing.T) {
	srv := newEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title,actors(name)&id=eq.2", nil)
	rows := decodeArray(t, resp)
	actors, ok := rows[0]["actors"].([]any)
	if !ok {
		t.Fatalf("actors = %#v, want an array", rows[0]["actors"])
	}
	if len(actors) != 2 {
		t.Fatalf("Blade Runner has %d actors, want 2", len(actors))
	}
	names := map[string]bool{}
	for _, a := range actors {
		names[a.(map[string]any)["name"].(string)] = true
	}
	if !names["Ford"] || !names["Hauer"] {
		t.Errorf("actors = %v, want Ford and Hauer", names)
	}
}

func TestEmbedInnerJoinFilters(t *testing.T) {
	srv := newEmbedServer(t)
	// !inner drops parents with no match: only films that have actors survive.
	resp := do(t, srv, http.MethodGet, "/films?select=title,actors!inner(name)&order=id", nil)
	rows := decodeArray(t, resp)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (only films with actors)", len(rows))
	}
	titles := []string{rows[0]["title"].(string), rows[1]["title"].(string)}
	if titles[0] != "Blade Runner" || titles[1] != "Arrival" {
		t.Errorf("titles = %v, want [Blade Runner Arrival]", titles)
	}
}

func TestEmbedScopedFilterAndOrder(t *testing.T) {
	srv := newEmbedServer(t)
	// An embed-scoped filter restricts the embedded array; ordering applies inside.
	resp := do(t, srv, http.MethodGet, "/films?select=title,actors(name)&actors.name=eq.Ford&id=eq.2", nil)
	rows := decodeArray(t, resp)
	actors := rows[0]["actors"].([]any)
	if len(actors) != 1 {
		t.Fatalf("got %d actors, want 1 after the embed filter", len(actors))
	}
	if actors[0].(map[string]any)["name"] != "Ford" {
		t.Errorf("actor = %v, want Ford", actors[0])
	}
}

func TestEmbedAliasKey(t *testing.T) {
	srv := newEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title,helmer:directors(name)&id=eq.1", nil)
	rows := decodeArray(t, resp)
	if _, has := rows[0]["helmer"]; !has {
		t.Fatalf("response has no aliased key helmer: %#v", rows[0])
	}
	if _, has := rows[0]["director"]; has {
		t.Error("response should use the alias helmer, not director")
	}
}

func TestEmbedNested(t *testing.T) {
	srv := newEmbedServer(t)
	// directors -> films -> actors, two levels deep.
	resp := do(t, srv, http.MethodGet, "/directors?select=name,films(title,actors(name))&id=eq.2", nil)
	rows := decodeArray(t, resp)
	films := rows[0]["films"].([]any)
	if len(films) != 1 {
		t.Fatalf("got %d films, want 1", len(films))
	}
	actors := films[0].(map[string]any)["actors"].([]any)
	if len(actors) != 2 {
		t.Errorf("nested actors = %d, want 2", len(actors))
	}
}

func TestEmbedNoRelationship(t *testing.T) {
	srv := newEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title,nonsense(x)", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env["code"] != "PGRST200" {
		t.Errorf("code = %v, want PGRST200", env["code"])
	}
}

func TestEmbedColumnInCSV(t *testing.T) {
	srv := newEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title,director:directors(name)&id=eq.1",
		map[string]string{"Accept": "text/csv"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// The embedded object lands as JSON text inside a single CSV cell; parsing the
	// record back out yields the JSON unescaped.
	recs, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("csv = %q (err %v)", raw, err)
	}
	if recs[1][1] != `{"name":"Lang"}` {
		t.Errorf("director cell = %q, want the embedded JSON object", recs[1][1])
	}
}

func BenchmarkEmbedToMany(b *testing.B) {
	srv := newEmbedServer(b)
	req := httptest.NewRequest(http.MethodGet, "/directors?select=name,films(title)&order=id", nil)
	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}
