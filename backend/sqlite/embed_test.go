package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/plan"
	"github.com/tamnd/dbrest/reqctx"
)

// openEmbed seeds two related tables (directors and films, with a films->directors
// foreign key) for the embedding tests at the backend layer.
func openEmbed(t *testing.T) *Backend {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
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
		INSERT INTO directors (id, name) VALUES (1, 'Lang'), (2, 'Scott');
		INSERT INTO films (id, title, director_id) VALUES
			(1, 'Metropolis', 1), (2, 'Blade Runner', 2), (3, 'Untitled', NULL);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return b
}

func TestIntrospectForeignKey(t *testing.T) {
	b := openEmbed(t)
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	films, _ := model.Lookup("films", nil)
	if len(films.ForeignKeys) != 1 {
		t.Fatalf("films has %d foreign keys, want 1", len(films.ForeignKeys))
	}
	fk := films.ForeignKeys[0]
	if fk.Name != "films_director_id_fkey" {
		t.Errorf("fk name = %q, want films_director_id_fkey", fk.Name)
	}
	if len(fk.Columns) != 1 || fk.Columns[0] != "director_id" {
		t.Errorf("fk columns = %v, want [director_id]", fk.Columns)
	}
	if fk.RefRelation != "directors" {
		t.Errorf("fk ref = %q, want directors", fk.RefRelation)
	}
	// SQLite reports a NULL "to" for a key referencing the parent primary key; the
	// introspector resolves it to the referenced PK column.
	if len(fk.RefColumns) != 1 || fk.RefColumns[0] != "id" {
		t.Errorf("fk ref columns = %v, want [id]", fk.RefColumns)
	}
}

// planEmbed parses, plans, and returns the resolved query for a films read with
// an embed expressed as a select string.
func planEmbed(t *testing.T, b *Backend, relation, query string) *ir.Query {
	t.Helper()
	q, perr := ir.ParseRead(relation, query, nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	pl, perr := plan.Read(model, q, nil)
	if perr != nil {
		t.Fatalf("plan.Read: %v", perr)
	}
	return pl.Query
}

func TestCompileEmbedToOneSQL(t *testing.T) {
	b := openEmbed(t)
	q := planEmbed(t, b, "films", "select=title,director:directors(name)")
	st, perr := sqlgen.CompileRead(dialect{}, q)
	if perr != nil {
		t.Fatalf("CompileRead: %v", perr)
	}
	// The parent is aliased and the to-one embed is a correlated object subquery
	// joining the target back to the parent, capped at one row.
	for _, want := range []string{
		`FROM "films" t0`,
		`json_object('name', t1."name")`,
		`FROM "directors" t1`,
		`WHERE t1."id" = t0."director_id"`,
		`LIMIT 1`,
		`AS "director"`,
	} {
		if !strings.Contains(st.SQL, want) {
			t.Errorf("SQL missing %q\n got: %s", want, st.SQL)
		}
	}
}

func TestCompileEmbedToManySQL(t *testing.T) {
	b := openEmbed(t)
	q := planEmbed(t, b, "directors", "select=name,films(title)")
	st, perr := sqlgen.CompileRead(dialect{}, q)
	if perr != nil {
		t.Fatalf("CompileRead: %v", perr)
	}
	for _, want := range []string{
		`FROM "directors" t0`,
		`json_group_array(json(je."__e"))`,
		`WHERE t1."director_id" = t0."id"`,
		`AS "films"`,
	} {
		if !strings.Contains(st.SQL, want) {
			t.Errorf("SQL missing %q\n got: %s", want, st.SQL)
		}
	}
}

func TestExecuteEmbedToOneValue(t *testing.T) {
	b := openEmbed(t)
	q := planEmbed(t, b, "films", "select=title,director:directors(name)&order=id")
	rows := execReadResolved(t, b, q)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// The embed column comes back as engine-assembled JSON text.
	if got, _ := asString(rows[0]["director"]); got != `{"name":"Lang"}` {
		t.Errorf("row0 director = %q, want {\"name\":\"Lang\"}", got)
	}
	// The film with no director carries a NULL embed column.
	if rows[2]["director"] != nil {
		t.Errorf("row2 director = %#v, want nil", rows[2]["director"])
	}
}

func TestExecuteEmbedToManyValue(t *testing.T) {
	b := openEmbed(t)
	q := planEmbed(t, b, "directors", "select=name,films(title)&order=id")
	rows := execReadResolved(t, b, q)
	if got, _ := asString(rows[0]["films"]); got != `[{"title":"Metropolis"}]` {
		t.Errorf("row0 films = %q", got)
	}
}

// execReadResolved executes an already-planned read and returns the rows. The
// query's relation reference is bound by the planner, so it runs as-is.
func execReadResolved(t *testing.T, b *Backend, q *ir.Query) []map[string]any {
	t.Helper()
	pl := &ir.Plan{Query: q, ReadOnly: true}
	res, err := b.Execute(context.Background(), pl, &reqctx.Context{Role: "anon"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return readAll(t, res)
}
