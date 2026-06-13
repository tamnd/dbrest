package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend/sqlgen"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/plan"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
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

// TestIntrospectUniqueConstraint covers 01.8 end-to-end on SQLite: a UNIQUE
// constraint on a foreign-key column is read from PRAGMA index_list/index_info,
// recorded on the relation, and makes the reverse embed one-to-one so it renders
// as an object rather than an array.
func TestIntrospectUniqueConstraint(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	b, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	_, err = b.DB().Exec(`
		CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE profiles (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL UNIQUE REFERENCES users(id),
			bio TEXT
		);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	profiles, _ := model.Lookup("profiles", nil)
	if len(profiles.Unique) != 1 || profiles.Unique[0][0] != "user_id" {
		t.Fatalf("profiles.Unique = %v, want [[user_id]]", profiles.Unique)
	}

	users, _ := model.Lookup("users", nil)
	cands, _ := model.Relationships(users, "profiles", nil)
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].Card != schema.CardToOne {
		t.Errorf("Card = %v, want to-one (user_id is unique)", cands[0].Card)
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
	pl, perr := plan.Read(model, q, nil, plan.Options{})
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

// openEmbedNull seeds directors where one (Welles) has no films, so an
// embed-existence filter has something to include and exclude.
func openEmbedNull(t *testing.T) *Backend {
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
		INSERT INTO directors (id, name) VALUES (1, 'Lang'), (2, 'Scott'), (3, 'Welles');
		INSERT INTO films (id, title, director_id) VALUES
			(1, 'Metropolis', 1), (2, 'Blade Runner', 2);
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return b
}

// names pulls the name column out of a result set in row order.
func names(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i], _ = asString(r["name"])
	}
	return out
}

// directors?select=name,films(title)&films=not.is.null keeps only directors with
// at least one film: a semi-join over the relationship (item 01.12).
func TestExecuteEmbedNotIsNull(t *testing.T) {
	b := openEmbedNull(t)
	q := planEmbed(t, b, "directors", "select=name,films(title)&films=not.is.null&order=id")
	got := names(execReadResolved(t, b, q))
	if len(got) != 2 || got[0] != "Lang" || got[1] != "Scott" {
		t.Errorf("not.is.null directors = %v, want [Lang Scott]", got)
	}
}

// directors?...&films=is.null is the anti-join: only the director with no films.
func TestExecuteEmbedIsNull(t *testing.T) {
	b := openEmbedNull(t)
	q := planEmbed(t, b, "directors", "select=name,films(title)&films=is.null&order=id")
	got := names(execReadResolved(t, b, q))
	if len(got) != 1 || got[0] != "Welles" {
		t.Errorf("is.null directors = %v, want [Welles]", got)
	}
}

// The predicate composes under or=(...): directors with a film OR named Welles is
// everyone here, exercising the EXISTS as one disjunct alongside a column compare.
func TestExecuteEmbedNullInsideOr(t *testing.T) {
	b := openEmbedNull(t)
	q := planEmbed(t, b, "directors", "select=name,films(title)&or=(films.not.is.null,name.eq.Welles)&order=id")
	got := names(execReadResolved(t, b, q))
	if len(got) != 3 {
		t.Errorf("or= directors = %v, want all three", got)
	}
}

// A count alongside the windowed read must apply the same semi-join, so the
// total reflects only the directors that have films.
func TestExecuteEmbedNullCount(t *testing.T) {
	b := openEmbedNull(t)
	q := planEmbed(t, b, "directors", "select=name,films(title)&films=not.is.null")
	st, perr := sqlgen.CompileCount(dialect{}, q)
	if perr != nil {
		t.Fatalf("CompileCount: %v", perr)
	}
	var n int
	if err := b.DB().QueryRow(st.SQL, st.Args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
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

// TestExecuteDeclaredRecursiveEmbed covers 01.10 end-to-end: a declared computed
// relationship names one direction of a self-referential foreign key, so the
// recursive embed compiles and executes, returning each comment's children.
func TestExecuteDeclaredRecursiveEmbed(t *testing.T) {
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	b, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	_, err = b.DB().Exec(`
		CREATE TABLE comments (
			id INTEGER PRIMARY KEY,
			parent_id INTEGER REFERENCES comments(id),
			body TEXT NOT NULL
		);
		INSERT INTO comments (id, parent_id, body) VALUES
			(1, NULL, 'root'), (2, 1, 'first reply'), (3, 1, 'second reply');
	`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	model, err := b.Introspect(context.Background())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	model.AddDeclaredRelationship(schema.DeclaredRel{
		Name:         "children",
		ParentSchema: "", ParentName: "comments",
		TargetSchema: "", TargetName: "comments",
		Card:    schema.CardToMany,
		Local:   []string{"id"},
		Foreign: []string{"parent_id"},
	})

	q, perr := ir.ParseRead("comments", "select=body,children:comments!children(body)&id=eq.1", nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	pl, perr := plan.Read(model, q, nil, plan.Options{})
	if perr != nil {
		t.Fatalf("plan.Read: %v", perr)
	}
	rows := execReadResolved(t, b, pl.Query)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	raw, ok := asString(rows[0]["children"])
	if !ok {
		t.Fatalf("children = %T, want JSON array text", rows[0]["children"])
	}
	var kids []map[string]any
	if err := json.Unmarshal([]byte(raw), &kids); err != nil {
		t.Fatalf("children is not a JSON array: %v (%q)", err, raw)
	}
	if len(kids) != 2 {
		t.Errorf("got %d children, want 2", len(kids))
	}
}
