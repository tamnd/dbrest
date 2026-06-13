package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlite"
	"github.com/tamnd/dbrest/httpapi"
	"github.com/tamnd/dbrest/rpc"
)

// rpcEmbedFunctions returns rows of known relations so the call result supports
// embedding: recent_films is setof films (a to-one director, a many-to-many
// actors), all_directors is setof directors (a to-many films), and film_titles
// is setof text, a scalar set with no relation to embed against.
func rpcEmbedFunctions() []*rpc.Function {
	return []*rpc.Function{
		{
			Name:       "recent_films",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: "films"},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT * FROM films ORDER BY id"},
		},
		{
			Name:       "all_directors",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: "directors"},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT * FROM directors ORDER BY id"},
		},
		{
			Name:       "film_titles",
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnSetOf, Type: "text"},
			Volatility: rpc.Stable,
			Query:      &rpc.PortableQuery{SQL: "SELECT title FROM films ORDER BY id"},
		},
	}
}

// newRPCEmbedServer seeds the canonical embedding fixture (directors, films,
// actors, roles) and registers functions that return rows of those relations, so
// /rpc embeds resolve through the same relationships a table read uses.
func newRPCEmbedServer(t testing.TB) *httpapi.Server {
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

	be.Register(rpc.NewStaticRegistry(rpcEmbedFunctions()))

	model, err := be.Introspect(context.Background())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	srv := httpapi.NewServer(be, model, nil)
	srv.SetDefaultRole("anon")
	return srv
}

// A function returning rows of a relation embeds its to-one relation: each film
// carries its director as a nested object, NULL when the film has no director.
func TestRPCEmbedToOne(t *testing.T) {
	srv := newRPCEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/recent_films?select=title,directors(name)&order=id", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	if rows[0]["title"] != "Metropolis" {
		t.Errorf("row 0 title = %v, want Metropolis", rows[0]["title"])
	}
	d, ok := rows[0]["directors"].(map[string]any)
	if !ok {
		t.Fatalf("directors = %#v, want a nested object", rows[0]["directors"])
	}
	if d["name"] != "Lang" {
		t.Errorf("director = %v, want Lang", d["name"])
	}
	// Film 4 has no director, so the to-one embed is JSON null.
	if rows[3]["directors"] != nil {
		t.Errorf("film 4 directors = %#v, want null", rows[3]["directors"])
	}
}

// A function result embeds a many-to-many relation: each film carries its actors
// as an array, empty for a film with no roles.
func TestRPCEmbedToMany(t *testing.T) {
	srv := newRPCEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/recent_films?select=title,actors(name)&order=id", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Blade Runner (id 2) has two actors.
	actors, ok := rows[1]["actors"].([]any)
	if !ok {
		t.Fatalf("actors = %#v, want an array", rows[1]["actors"])
	}
	if len(actors) != 2 {
		t.Fatalf("Blade Runner has %d actors, want 2", len(actors))
	}
	// Metropolis (id 1) has no actors: an empty array, not null.
	empty, ok := rows[0]["actors"].([]any)
	if !ok || len(empty) != 0 {
		t.Errorf("Metropolis actors = %#v, want an empty array", rows[0]["actors"])
	}
}

// An !inner embed on a call drops parent rows with no related match, the same as
// on a table read: only films that have actors survive.
func TestRPCEmbedInnerFilters(t *testing.T) {
	srv := newRPCEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/recent_films?select=title,actors!inner(name)&order=id", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (only films with actors)", len(rows))
	}
}

// A to-many embed from the other side: each director carries its films.
func TestRPCEmbedToManyFilms(t *testing.T) {
	srv := newRPCEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/all_directors?select=name,films(title)&order=id", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	films, ok := rows[0]["films"].([]any)
	if !ok || len(films) != 1 {
		t.Fatalf("director 1 films = %#v, want one film", rows[0]["films"])
	}
	if films[0].(map[string]any)["title"] != "Metropolis" {
		t.Errorf("film = %v, want Metropolis", films[0])
	}
}

// An exact count on an embedded call carries the embed's restriction: with
// actors!inner only the two films that have actors count, and Content-Range
// reports that total rather than the function's full row set.
func TestRPCEmbedInnerCount(t *testing.T) {
	srv := newRPCEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/recent_films?select=title,actors!inner(name)&order=id",
		map[string]string{"Prefer": "count=exact"})
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 200/206", resp.StatusCode)
	}
	cr := resp.Header.Get("Content-Range")
	if !strings.HasSuffix(cr, "/2") {
		t.Errorf("Content-Range = %q, want a total of 2", cr)
	}
}

// Embedding on a function whose result is not a known relation has nothing to
// resolve against and is the read path's PGRST200.
func TestRPCEmbedOnScalarSetIsError(t *testing.T) {
	srv := newRPCEmbedServer(t)
	resp := do(t, srv, http.MethodGet, "/rpc/film_titles?select=directors(name)", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var env map[string]any
	json.NewDecoder(resp.Body).Decode(&env)
	if env["code"] != "PGRST200" {
		t.Errorf("code = %v, want PGRST200", env["code"])
	}
}
