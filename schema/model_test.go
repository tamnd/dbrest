package schema

import "testing"

func sampleModel() *Model {
	return NewModel([]*Relation{
		{Schema: "public", Name: "users", Kind: KindTable, PrimaryKey: []string{"id"}, Columns: []*Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "name", Type: "text", Nullable: true, Position: 2},
		}},
		{Schema: "public", Name: "todos", Kind: KindTable, Columns: []*Column{
			{Name: "id", Type: "integer", Position: 1},
		}},
		{Schema: "private", Name: "secrets", Kind: KindTable, Columns: []*Column{
			{Name: "id", Type: "integer", Position: 1},
		}},
	})
}

func TestKey(t *testing.T) {
	if got := Key("", "users"); got != "users" {
		t.Errorf("unqualified Key = %q, want %q", got, "users")
	}
	if got := Key("public", "users"); got != "public.users" {
		t.Errorf("qualified Key = %q, want %q", got, "public.users")
	}
}

func TestLookupSearchPath(t *testing.T) {
	m := sampleModel()

	// Unqualified resolves via search path.
	r, ok := m.Lookup("users", []string{"public"})
	if !ok || r.Name != "users" {
		t.Fatalf("Lookup(users) = %v, %v", r, ok)
	}

	// A relation only in a non-searched schema is invisible unqualified.
	if _, ok := m.Lookup("secrets", []string{"public"}); ok {
		t.Error("secrets should not resolve when only public is searched")
	}

	// An empty search path matches model keys directly, the
	// introspection-internal mode.
	if _, ok := m.Lookup("private.secrets", nil); !ok {
		t.Error("private.secrets should resolve against the model key with no search path")
	}

	// Unknown stays unknown.
	if _, ok := m.Lookup("nope", []string{"public"}); ok {
		t.Error("unknown relation should not resolve")
	}

	// A request never reaches outside its active schema with a dotted name:
	// the path segment is a bare name within the schema, never a qualified
	// reference (PostgREST profile semantics).
	if _, ok := m.Lookup("private.secrets", []string{"public"}); ok {
		t.Error("a dotted name must not escape the active schema")
	}
	if _, ok := m.Lookup("public.users", []string{"public"}); ok {
		t.Error("a dotted name must not bypass search-path keying")
	}
}

func TestRelationsIn(t *testing.T) {
	m := sampleModel()

	pub := m.RelationsIn("public")
	if len(pub) != 2 || pub[0].Name != "users" || pub[1].Name != "todos" {
		t.Fatalf("RelationsIn(public) = %v, want [users todos]", names(pub))
	}
	priv := m.RelationsIn("private")
	if len(priv) != 1 || priv[0].Name != "secrets" {
		t.Fatalf("RelationsIn(private) = %v, want [secrets]", names(priv))
	}
	if got := m.RelationsIn("nope"); len(got) != 0 {
		t.Errorf("RelationsIn(nope) = %v, want empty", names(got))
	}
}

func names(rels []*Relation) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = r.Name
	}
	return out
}

func TestColumnLookup(t *testing.T) {
	m := sampleModel()
	r, _ := m.Lookup("users", []string{"public"})

	if !r.HasColumn("name") {
		t.Error("users should have column name")
	}
	if r.HasColumn("missing") {
		t.Error("users should not report a missing column")
	}
	c, ok := r.Column("id")
	if !ok || c.Position != 1 {
		t.Errorf("Column(id) = %v, %v", c, ok)
	}
}

func TestRelationsDeterministicOrder(t *testing.T) {
	m := sampleModel()
	rels := m.Relations()
	if len(rels) != 3 {
		t.Fatalf("Relations len = %d, want 3", len(rels))
	}
	want := []string{"users", "todos", "secrets"}
	for i, r := range rels {
		if r.Name != want[i] {
			t.Errorf("Relations()[%d].Name = %q, want %q", i, r.Name, want[i])
		}
	}
	if m.Len() != 3 {
		t.Errorf("Len = %d, want 3", m.Len())
	}
}
