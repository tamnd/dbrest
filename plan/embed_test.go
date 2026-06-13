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
