package schema

import "testing"

// buildEmbedModel wires the canonical embedding fixture: films reference one
// director (forward FK, to-one), directors own many films (the reverse view,
// to-many), and films relate to actors through the roles junction (many-to-many).
func buildEmbedModel() *Model {
	directors := &Relation{Schema: "public", Name: "directors", Columns: cols("id", "name")}
	films := &Relation{
		Schema:  "public",
		Name:    "films",
		Columns: cols("id", "title", "director_id"),
		ForeignKeys: []*ForeignKey{{
			Name: "films_director_id_fkey", Columns: []string{"director_id"},
			RefSchema: "public", RefRelation: "directors", RefColumns: []string{"id"},
		}},
	}
	actors := &Relation{Schema: "public", Name: "actors", Columns: cols("id", "name")}
	roles := &Relation{
		Schema:     "public",
		Name:       "roles",
		Columns:    cols("film_id", "actor_id"),
		PrimaryKey: []string{"film_id", "actor_id"}, // the composite PK that makes roles a junction
		ForeignKeys: []*ForeignKey{
			{Name: "roles_film_id_fkey", Columns: []string{"film_id"}, RefSchema: "public", RefRelation: "films", RefColumns: []string{"id"}},
			{Name: "roles_actor_id_fkey", Columns: []string{"actor_id"}, RefSchema: "public", RefRelation: "actors", RefColumns: []string{"id"}},
		},
	}
	return NewModel([]*Relation{directors, films, actors, roles})
}

func cols(names ...string) []*Column {
	out := make([]*Column, len(names))
	for i, n := range names {
		out[i] = &Column{Name: n, Type: "text", Position: i + 1}
	}
	return out
}

func rel(t *testing.T, m *Model, name string) *Relation {
	t.Helper()
	r, ok := m.Lookup(name, []string{"public"})
	if !ok {
		t.Fatalf("relation %q not in model", name)
	}
	return r
}

func TestRelationshipsForwardToOne(t *testing.T) {
	m := buildEmbedModel()
	cands, found := m.Relationships(rel(t, m, "films"), "directors", []string{"public"})
	if !found {
		t.Fatal("directors not found")
	}
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.Card != CardToOne {
		t.Errorf("Card = %v, want to-one", c.Card)
	}
	if got := c.Local; len(got) != 1 || got[0] != "director_id" {
		t.Errorf("Local = %v, want [director_id]", got)
	}
	if got := c.Foreign; len(got) != 1 || got[0] != "id" {
		t.Errorf("Foreign = %v, want [id]", got)
	}
}

func TestRelationshipsBackwardToMany(t *testing.T) {
	m := buildEmbedModel()
	cands, _ := m.Relationships(rel(t, m, "directors"), "films", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.Card != CardToMany {
		t.Errorf("Card = %v, want to-many", c.Card)
	}
	// The reverse view joins the director's id to the film's director_id.
	if c.Local[0] != "id" || c.Foreign[0] != "director_id" {
		t.Errorf("join = %v -> %v, want [id] -> [director_id]", c.Local, c.Foreign)
	}
}

func TestRelationshipsManyToManyJunction(t *testing.T) {
	m := buildEmbedModel()
	cands, _ := m.Relationships(rel(t, m, "films"), "actors", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	c := cands[0]
	if c.Card != CardToMany {
		t.Errorf("Card = %v, want to-many", c.Card)
	}
	if c.Junction == nil || c.Junction.Name != "roles" {
		t.Fatalf("Junction = %v, want roles", c.Junction)
	}
	if c.Local[0] != "id" || c.JLocal[0] != "film_id" {
		t.Errorf("parent hop = %v = %v, want id = film_id", c.Local, c.JLocal)
	}
	if c.JForeign[0] != "actor_id" || c.Foreign[0] != "id" {
		t.Errorf("target hop = %v = %v, want actor_id = id", c.JForeign, c.Foreign)
	}
}

func TestRelationshipsTargetMissing(t *testing.T) {
	m := buildEmbedModel()
	_, found := m.Relationships(rel(t, m, "films"), "nope", []string{"public"})
	if found {
		t.Error("found should be false for an unknown target")
	}
}

func TestRelationshipsNoEdge(t *testing.T) {
	m := buildEmbedModel()
	// directors and actors share no foreign key and no junction between them.
	cands, found := m.Relationships(rel(t, m, "directors"), "actors", []string{"public"})
	if !found {
		t.Fatal("actors exists, found should be true")
	}
	if len(cands) != 0 {
		t.Errorf("got %d candidates, want 0", len(cands))
	}
}

func TestRelationshipsAmbiguous(t *testing.T) {
	// Two foreign keys from films to people (director and writer) make an
	// unqualified embed of people ambiguous.
	people := &Relation{Schema: "public", Name: "people", Columns: cols("id", "name")}
	films := &Relation{
		Schema: "public", Name: "films", Columns: cols("id", "director_id", "writer_id"),
		ForeignKeys: []*ForeignKey{
			{Name: "films_director_id_fkey", Columns: []string{"director_id"}, RefSchema: "public", RefRelation: "people", RefColumns: []string{"id"}},
			{Name: "films_writer_id_fkey", Columns: []string{"writer_id"}, RefSchema: "public", RefRelation: "people", RefColumns: []string{"id"}},
		},
	}
	m := NewModel([]*Relation{people, films})
	cands, _ := m.Relationships(rel(t, m, "films"), "people", []string{"public"})
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2", len(cands))
	}
	// A hint on the writer column selects exactly one.
	matched := 0
	for _, c := range cands {
		if c.MatchesHint("writer_id") {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("writer_id hint matched %d edges, want 1", matched)
	}
}

// TestRelationshipsReverseToOneOnPrimaryKey covers 01.8: a foreign key whose
// columns are the referencing relation's primary key is one-to-one, so its
// reverse view renders as an object. profiles.user_id is both the PK of
// profiles and an FK to users, so a user has at most one profile.
func TestRelationshipsReverseToOneOnPrimaryKey(t *testing.T) {
	users := &Relation{Schema: "public", Name: "users", Columns: cols("id", "name")}
	profiles := &Relation{
		Schema:     "public",
		Name:       "profiles",
		Columns:    cols("user_id", "bio"),
		PrimaryKey: []string{"user_id"},
		ForeignKeys: []*ForeignKey{{
			Name: "profiles_user_id_fkey", Columns: []string{"user_id"},
			RefSchema: "public", RefRelation: "users", RefColumns: []string{"id"},
		}},
	}
	m := NewModel([]*Relation{users, profiles})
	cands, _ := m.Relationships(rel(t, m, "users"), "profiles", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].Card != CardToOne {
		t.Errorf("Card = %v, want to-one (FK is the profiles PK)", cands[0].Card)
	}
}

// TestRelationshipsReverseToOneOnUniqueConstraint covers 01.8 via a unique
// constraint rather than the primary key: profiles has its own surrogate PK,
// but a UNIQUE(user_id) constraint still makes the FK one-to-one.
func TestRelationshipsReverseToOneOnUniqueConstraint(t *testing.T) {
	users := &Relation{Schema: "public", Name: "users", Columns: cols("id", "name")}
	profiles := &Relation{
		Schema:     "public",
		Name:       "profiles",
		Columns:    cols("id", "user_id", "bio"),
		PrimaryKey: []string{"id"},
		Unique:     [][]string{{"user_id"}},
		ForeignKeys: []*ForeignKey{{
			Name: "profiles_user_id_fkey", Columns: []string{"user_id"},
			RefSchema: "public", RefRelation: "users", RefColumns: []string{"id"},
		}},
	}
	m := NewModel([]*Relation{users, profiles})
	cands, _ := m.Relationships(rel(t, m, "users"), "profiles", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].Card != CardToOne {
		t.Errorf("Card = %v, want to-one (FK matches a unique constraint)", cands[0].Card)
	}
}

// TestRelationshipsReverseToManyWithoutUnique covers the 01.8 negative: a
// plain FK that is neither the PK nor unique stays to-many, the ordinary
// reverse-view case (a director owns many films).
func TestRelationshipsReverseToManyWithoutUnique(t *testing.T) {
	m := buildEmbedModel()
	cands, _ := m.Relationships(rel(t, m, "directors"), "films", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].Card != CardToMany {
		t.Errorf("Card = %v, want to-many (FK is neither PK nor unique)", cands[0].Card)
	}
}

// TestRelationshipsIncidentalReferencingTableNotJunction covers 01.9: a table
// that has foreign keys to both ends but does not key them as its primary key
// is an incidental referencing table, not a junction, so it yields no edge.
// Here log references both films and actors but keys on its own id.
func TestRelationshipsIncidentalReferencingTableNotJunction(t *testing.T) {
	films := &Relation{Schema: "public", Name: "films", Columns: cols("id", "title")}
	actors := &Relation{Schema: "public", Name: "actors", Columns: cols("id", "name")}
	log := &Relation{
		Schema:     "public",
		Name:       "log",
		Columns:    cols("id", "film_id", "actor_id"),
		PrimaryKey: []string{"id"}, // keyed on its own surrogate id, not the FK pair
		ForeignKeys: []*ForeignKey{
			{Name: "log_film_id_fkey", Columns: []string{"film_id"}, RefSchema: "public", RefRelation: "films", RefColumns: []string{"id"}},
			{Name: "log_actor_id_fkey", Columns: []string{"actor_id"}, RefSchema: "public", RefRelation: "actors", RefColumns: []string{"id"}},
		},
	}
	m := NewModel([]*Relation{films, actors, log})
	cands, _ := m.Relationships(rel(t, m, "films"), "actors", []string{"public"})
	if len(cands) != 0 {
		t.Fatalf("got %d candidates, want 0 (log is not a junction)", len(cands))
	}
}

// TestRelationshipsJunctionWithExtraPrimaryKeyColumn covers 01.9: the FK
// columns only need to be a subset of the composite primary key, so a junction
// that adds another column to its PK (here a role discriminator) still embeds.
func TestRelationshipsJunctionWithExtraPrimaryKeyColumn(t *testing.T) {
	films := &Relation{Schema: "public", Name: "films", Columns: cols("id", "title")}
	actors := &Relation{Schema: "public", Name: "actors", Columns: cols("id", "name")}
	roles := &Relation{
		Schema:     "public",
		Name:       "roles",
		Columns:    cols("film_id", "actor_id", "character"),
		PrimaryKey: []string{"film_id", "actor_id", "character"},
		ForeignKeys: []*ForeignKey{
			{Name: "roles_film_id_fkey", Columns: []string{"film_id"}, RefSchema: "public", RefRelation: "films", RefColumns: []string{"id"}},
			{Name: "roles_actor_id_fkey", Columns: []string{"actor_id"}, RefSchema: "public", RefRelation: "actors", RefColumns: []string{"id"}},
		},
	}
	m := NewModel([]*Relation{films, actors, roles})
	cands, _ := m.Relationships(rel(t, m, "films"), "actors", []string{"public"})
	if len(cands) != 1 {
		t.Fatalf("got %d candidates, want 1", len(cands))
	}
	if cands[0].Junction == nil || cands[0].Junction.Name != "roles" {
		t.Fatalf("Junction = %v, want roles", cands[0].Junction)
	}
}

// TestRelationshipsSelfJunctionTwoKeys covers 01.9 with a self-referential
// many-to-many: a friendship junction has two FKs to users, which yields two
// distinct edges (one per direction), so an unqualified embed is ambiguous and
// a column hint disambiguates it.
func TestRelationshipsSelfJunctionTwoKeys(t *testing.T) {
	users := &Relation{Schema: "public", Name: "users", Columns: cols("id", "name")}
	friendships := &Relation{
		Schema:     "public",
		Name:       "friendships",
		Columns:    cols("user_id", "friend_id"),
		PrimaryKey: []string{"user_id", "friend_id"},
		ForeignKeys: []*ForeignKey{
			{Name: "friendships_user_id_fkey", Columns: []string{"user_id"}, RefSchema: "public", RefRelation: "users", RefColumns: []string{"id"}},
			{Name: "friendships_friend_id_fkey", Columns: []string{"friend_id"}, RefSchema: "public", RefRelation: "users", RefColumns: []string{"id"}},
		},
	}
	m := NewModel([]*Relation{users, friendships})
	cands, _ := m.Relationships(rel(t, m, "users"), "users", []string{"public"})
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2 (the two junction directions)", len(cands))
	}
	matched := 0
	for _, c := range cands {
		if c.MatchesHint("friend_id") {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("friend_id hint matched %d edges, want 1", matched)
	}
}
