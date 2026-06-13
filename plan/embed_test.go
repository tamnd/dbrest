package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/schema"
)

// embedModel wires films with two foreign keys to people (director and writer),
// so an unqualified people embed is ambiguous and a hinted one resolves.
func embedModel() *schema.Model {
	people := &schema.Relation{Name: "people", Columns: []*schema.Column{
		{Name: "id", Type: "integer", Position: 1},
		{Name: "name", Type: "text", Position: 2},
	}}
	films := &schema.Relation{
		Name: "films",
		Columns: []*schema.Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "title", Type: "text", Position: 2},
			{Name: "director_id", Type: "integer", Position: 3},
			{Name: "writer_id", Type: "integer", Position: 4},
		},
		ForeignKeys: []*schema.ForeignKey{
			{Name: "films_director_id_fkey", Columns: []string{"director_id"}, RefRelation: "people", RefColumns: []string{"id"}},
			{Name: "films_writer_id_fkey", Columns: []string{"writer_id"}, RefRelation: "people", RefColumns: []string{"id"}},
		},
	}
	return schema.NewModel([]*schema.Relation{people, films})
}

// readEmbed parses and plans a films read with the given select string.
func readEmbed(t *testing.T, m *schema.Model, sel string) (*ir.Plan, *pgerr.APIError) {
	t.Helper()
	q, perr := ir.ParseRead("films", "select="+sel, nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	return Read(m, q, nil, Options{})
}

func TestEmbedResolvesAndBinds(t *testing.T) {
	m := embedModel()
	pl, err := readEmbed(t, m, "title,director_id:people!director_id(name)")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	emb := pl.Query.Embeds[0]
	if emb.Rel == nil {
		t.Fatal("embed relationship not bound")
	}
	if emb.Cardinality != ir.CardToOne {
		t.Errorf("cardinality = %v, want to-one", emb.Cardinality)
	}
	if emb.Query.Relation.Name != "people" {
		t.Errorf("embed relation = %q, want people", emb.Query.Relation.Name)
	}
}

func TestEmbedNoRelationshipIsPGRST200(t *testing.T) {
	m := embedModel()
	_, err := readEmbed(t, m, "title,ghosts(x)")
	if err == nil || err.Code != "PGRST200" {
		t.Fatalf("want PGRST200, got %v", err)
	}
	// PGRST200 names the searched pair in its details (item 04.4), so a client
	// learns which relationship was looked for, not just that one was missing.
	if err.Details == nil {
		t.Fatal("PGRST200 details are nil, want the searched-pair sentence")
	}
	want := "Searched for a foreign key relationship between 'films' and 'ghosts'"
	if !strings.Contains(*err.Details, want) || !strings.Contains(*err.Details, "but no matches were found.") {
		t.Errorf("details = %q, want it to contain %q", *err.Details, want)
	}
}

// A PGRST200 raised for a hinted embed echoes the hint in the details, so a
// client sees the search was constrained by the hint it gave (item 04.4).
func TestEmbedNoRelationshipWithHintEchoesHint(t *testing.T) {
	m := embedModel()
	_, err := readEmbed(t, m, "title,ghosts!nope(x)")
	if err == nil || err.Code != "PGRST200" {
		t.Fatalf("want PGRST200, got %v", err)
	}
	if err.Details == nil || !strings.Contains(*err.Details, "using the hint 'nope'") {
		t.Errorf("details = %v, want it to echo the hint", err.Details)
	}
}

func TestEmbedAmbiguousIsPGRST201(t *testing.T) {
	m := embedModel()
	_, err := readEmbed(t, m, "title,people(name)")
	if err == nil || err.Code != "PGRST201" {
		t.Fatalf("want PGRST201, got %v", err)
	}
	// PostgREST returns 300 Multiple Choices for an ambiguous embed.
	if err.HTTPStatus != 300 {
		t.Errorf("status = %d, want 300", err.HTTPStatus)
	}
	// The details carry the candidate array a client reads to auto-disambiguate
	// (item 04.4): one entry per surviving edge, each with its cardinality, the
	// "parent with target" embedding, and the join-column relationship spelling.
	if err.RawDetails == nil {
		t.Fatal("PGRST201 details are nil, want the candidate array")
	}
	var cands []map[string]string
	if uerr := json.Unmarshal(err.RawDetails, &cands); uerr != nil {
		t.Fatalf("details is not a JSON array: %v: %s", uerr, err.RawDetails)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2: %v", len(cands), cands)
	}
	byRel := map[string]map[string]string{}
	for _, c := range cands {
		byRel[c["relationship"]] = c
	}
	want := "films_director_id_fkey using films(director_id) and people(id)"
	got, ok := byRel[want]
	if !ok {
		t.Fatalf("no candidate with relationship %q, got %v", want, cands)
	}
	if got["cardinality"] != "many-to-one" {
		t.Errorf("cardinality = %q, want many-to-one", got["cardinality"])
	}
	if got["embedding"] != "films with people" {
		t.Errorf("embedding = %q, want %q", got["embedding"], "films with people")
	}
	// The hint lists each disambiguated embed spelling and points at the details.
	if err.Hint == nil {
		t.Fatal("PGRST201 hint is nil, want the Try-changing list")
	}
	for _, frag := range []string{
		"Try changing 'people' to one of the following:",
		"'people!films_director_id_fkey'",
		"'people!films_writer_id_fkey'",
		"Find the desired relationship in the 'details' key.",
	} {
		if !strings.Contains(*err.Hint, frag) {
			t.Errorf("hint = %q, missing %q", *err.Hint, frag)
		}
	}
}

func TestEmbedHintDisambiguates(t *testing.T) {
	m := embedModel()
	if _, err := readEmbed(t, m, "title,people!writer_id(name)"); err != nil {
		t.Fatalf("a hinted embed should resolve, got %v", err)
	}
}

func TestEmbedUnknownColumnInEmbedIsRejected(t *testing.T) {
	m := embedModel()
	_, err := readEmbed(t, m, "title,people!director_id(nope)")
	// An unknown column inside an embed's select reaches PostgreSQL: 42703
	// (item 04.5), not the schema-cache PGRST204.
	if err == nil || err.Code != "42703" {
		t.Fatalf("want 42703, got %v", err)
	}
}

// commentsModel is a self-referential thread: comments.parent_id references
// comments.id, so the derived forward (parent) and backward (children) edges
// share a hint set and a bare or hinted embed is ambiguous (PGRST201).
func commentsModel() *schema.Model {
	comments := &schema.Relation{
		Name: "comments",
		Columns: []*schema.Column{
			{Name: "id", Type: "integer", Position: 1},
			{Name: "parent_id", Type: "integer", Position: 2},
			{Name: "body", Type: "text", Position: 3},
		},
		PrimaryKey: []string{"id"},
		ForeignKeys: []*schema.ForeignKey{
			{Name: "comments_parent_id_fkey", Columns: []string{"parent_id"}, RefRelation: "comments", RefColumns: []string{"id"}},
		},
	}
	return schema.NewModel([]*schema.Relation{comments})
}

// TestEmbedSelfReferentialIsAmbiguous covers 01.10: a self FK alone leaves the
// recursive embed ambiguous, with no hint able to pick a direction.
func TestEmbedSelfReferentialIsAmbiguous(t *testing.T) {
	m := commentsModel()
	q, perr := ir.ParseRead("comments", "select=body,comments(body)", nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	_, err := Read(m, q, nil, Options{})
	if err == nil || err.Code != "PGRST201" {
		t.Fatalf("want PGRST201, got %v", err)
	}
}

// TestEmbedDeclaredEdgeResolvesRecursive covers the 01.10 escape hatch: a
// declared computed relationship names one direction of the self FK, so the
// recursive embed resolves and binds with the declared cardinality.
func TestEmbedDeclaredEdgeResolvesRecursive(t *testing.T) {
	m := commentsModel()
	m.AddDeclaredRelationship(schema.DeclaredRel{
		Name:         "children",
		ParentSchema: "", ParentName: "comments",
		TargetSchema: "", TargetName: "comments",
		Card:    schema.CardToMany,
		Local:   []string{"id"},
		Foreign: []string{"parent_id"},
	})
	q, perr := ir.ParseRead("comments", "select=body,children:comments!children(body)", nil)
	if perr != nil {
		t.Fatalf("ParseRead: %v", perr)
	}
	pl, err := Read(m, q, nil, Options{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	emb := pl.Query.Embeds[0]
	if emb.Rel == nil || emb.Rel.Name != "children" {
		t.Fatalf("embed edge = %v, want children", emb.Rel)
	}
	if emb.Cardinality != ir.CardToMany {
		t.Errorf("cardinality = %v, want to-many", emb.Cardinality)
	}
}
