package sqlite

import (
	"context"
	"testing"
)

// baseColsStub answers the base-column lookup the view parser uses to expand a
// star projection, for the unit tests that exercise the parser directly.
func baseColsStub(byTable map[string][]string) func(string) ([]string, bool) {
	return func(name string) ([]string, bool) {
		cols, ok := byTable[name]
		return cols, ok
	}
}

func TestParseViewColumnsStarProjection(t *testing.T) {
	ddl := `CREATE VIEW film_view AS SELECT * FROM films`
	got := parseViewColumns(ddl, baseColsStub(map[string][]string{
		"films": {"id", "title", "director_id"},
	}))
	if len(got) != 3 {
		t.Fatalf("got %d view columns, want 3", len(got))
	}
	if got[2].Name != "director_id" || got[2].BaseColumn != "director_id" || got[2].BaseRelation != "films" {
		t.Errorf("third column = %+v, want director_id<-films.director_id", got[2])
	}
}

func TestParseViewColumnsExplicitListWithAlias(t *testing.T) {
	ddl := `CREATE VIEW v AS SELECT id, director_id AS dir FROM films`
	got := parseViewColumns(ddl, baseColsStub(nil))
	if len(got) != 2 {
		t.Fatalf("got %d view columns, want 2", len(got))
	}
	if got[1].Name != "dir" || got[1].BaseColumn != "director_id" {
		t.Errorf("aliased column = %+v, want dir<-director_id", got[1])
	}
}

func TestParseViewColumnsQualifiedReference(t *testing.T) {
	ddl := `CREATE VIEW v AS SELECT f.id, f.director_id FROM films f`
	got := parseViewColumns(ddl, baseColsStub(nil))
	if len(got) != 2 {
		t.Fatalf("got %d view columns, want 2", len(got))
	}
	if got[1].BaseColumn != "director_id" || got[1].Name != "director_id" {
		t.Errorf("qualified column = %+v, want director_id", got[1])
	}
}

func TestParseViewColumnsRejectsJoin(t *testing.T) {
	ddl := `CREATE VIEW v AS SELECT f.id FROM films f JOIN directors d ON d.id = f.director_id`
	if got := parseViewColumns(ddl, baseColsStub(nil)); got != nil {
		t.Errorf("a joined view should not project, got %v", got)
	}
}

func TestParseViewColumnsRejectsUnion(t *testing.T) {
	ddl := `CREATE VIEW v AS SELECT id FROM films UNION SELECT id FROM directors`
	if got := parseViewColumns(ddl, baseColsStub(nil)); got != nil {
		t.Errorf("a union view should not project, got %v", got)
	}
}

func TestParseViewColumnsRejectsExpression(t *testing.T) {
	ddl := `CREATE VIEW v AS SELECT id, upper(title) AS t FROM films`
	if got := parseViewColumns(ddl, baseColsStub(nil)); got != nil {
		t.Errorf("an expression column should stop projection, got %v", got)
	}
}

// TestExecuteEmbedThroughView covers 01.11 end-to-end on SQLite: a view defined
// as SELECT over a base table inherits the base foreign key, so the view embeds
// the referenced table as a to-one and returns the nested object.
func TestExecuteEmbedThroughView(t *testing.T) {
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	b, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	_, err = b.DB().Exec(`
		CREATE TABLE directors (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE films (
			id INTEGER PRIMARY KEY,
			title TEXT NOT NULL,
			director_id INTEGER REFERENCES directors(id)
		);
		CREATE VIEW film_view AS SELECT id, title, director_id FROM films;
		INSERT INTO directors (id, name) VALUES (1, 'Lang');
		INSERT INTO films (id, title, director_id) VALUES (1, 'Metropolis', 1);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	view, _ := model.Lookup("film_view", nil)
	if len(view.ViewColumns) != 3 {
		t.Fatalf("film_view has %d view columns, want 3", len(view.ViewColumns))
	}
	cands, _ := model.Relationships(view, "directors", nil)
	if len(cands) != 1 {
		t.Fatalf("got %d relationships film_view->directors, want 1", len(cands))
	}
	q := planEmbed(t, b, "film_view", "select=title,director:directors(name)")
	rows := execReadResolved(t, b, q)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	dir, ok := asString(rows[0]["director"])
	if !ok || dir == "" {
		t.Fatalf("director = %v, want a nested object", rows[0]["director"])
	}
}
