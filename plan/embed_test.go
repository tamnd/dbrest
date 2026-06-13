package plan

import (
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
	if err == nil || err.Code != "PGRST204" {
		t.Fatalf("want PGRST204, got %v", err)
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
