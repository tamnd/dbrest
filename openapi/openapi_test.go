package openapi

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

// filmsModel is a two-relation model: a films table with a primary key and a
// directors table films references by foreign key.
func filmsModel() *schema.Model {
	films := &schema.Relation{
		Name: "films",
		Kind: schema.KindTable,
		Columns: []*schema.Column{
			{Name: "id", Type: "integer", Nullable: false, Position: 1},
			{Name: "title", Type: "text", Nullable: false, Position: 2},
			{Name: "year", Type: "integer", Nullable: true, Position: 3},
			{Name: "director_id", Type: "integer", Nullable: true, Position: 4},
		},
		PrimaryKey: []string{"id"},
		ForeignKeys: []*schema.ForeignKey{
			{Name: "films_director_id_fkey", Columns: []string{"director_id"}, RefRelation: "directors", RefColumns: []string{"id"}},
		},
	}
	directors := &schema.Relation{
		Name: "directors",
		Kind: schema.KindTable,
		Columns: []*schema.Column{
			{Name: "id", Type: "integer", Nullable: false, Position: 1},
			{Name: "name", Type: "text", Nullable: false, Position: 2},
		},
		PrimaryKey: []string{"id"},
	}
	return schema.NewModel([]*schema.Relation{films, directors})
}

// sqliteCaps mirrors the SQLite backend's grades: regex and full-text present,
// array/range operators unsupported.
func sqliteCaps() backend.Capabilities {
	return backend.Capabilities{
		Regex:           backend.Native,
		FullText:        backend.FTSQLite5,
		IsDistinctFrom:  backend.Native,
		ArrayRangeTypes: backend.Unsupported,
	}
}

// decode generates the document and unmarshals it for structural assertions.
func decode(t *testing.T, model *schema.Model, fns rpc.Registry, caps backend.Capabilities, opts Options) map[string]any {
	t.Helper()
	body, err := Generate(model, fns, caps, opts)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

func TestGenerateShape(t *testing.T) {
	doc := decode(t, filmsModel(), nil, sqliteCaps(), Options{Host: "localhost:3000"})
	if doc["swagger"] != "2.0" {
		t.Errorf("swagger = %v, want 2.0", doc["swagger"])
	}
	info := doc["info"].(map[string]any)
	if info["title"] != "dbrest" || info["version"] != "14.0" {
		t.Errorf("info = %v, want dbrest/14.0", info)
	}
	if doc["host"] != "localhost:3000" {
		t.Errorf("host = %v", doc["host"])
	}
	paths := doc["paths"].(map[string]any)
	if _, ok := paths["/films"]; !ok {
		t.Error("missing /films path")
	}
	if _, ok := paths["/directors"]; !ok {
		t.Error("missing /directors path")
	}
}

func TestTableHasFullOperationSet(t *testing.T) {
	doc := decode(t, filmsModel(), nil, sqliteCaps(), Options{})
	films := doc["paths"].(map[string]any)["/films"].(map[string]any)
	for _, op := range []string{"get", "post", "patch", "delete"} {
		if _, ok := films[op]; !ok {
			t.Errorf("/films missing %s operation", op)
		}
	}
}

func TestViewIsReadOnly(t *testing.T) {
	rel := &schema.Relation{
		Name: "v_films", Kind: schema.KindView,
		Columns: []*schema.Column{{Name: "title", Type: "text", Position: 1}},
	}
	doc := decode(t, schema.NewModel([]*schema.Relation{rel}), nil, sqliteCaps(), Options{})
	v := doc["paths"].(map[string]any)["/v_films"].(map[string]any)
	if _, ok := v["get"]; !ok {
		t.Error("view should have get")
	}
	for _, op := range []string{"post", "patch", "delete"} {
		if _, ok := v[op]; ok {
			t.Errorf("read-only view should not have %s", op)
		}
	}
}

func TestDefinitionTypesAndRequired(t *testing.T) {
	doc := decode(t, filmsModel(), nil, sqliteCaps(), Options{})
	films := doc["definitions"].(map[string]any)["films"].(map[string]any)
	props := films["properties"].(map[string]any)

	id := props["id"].(map[string]any)
	if id["type"] != "integer" || id["format"] != "integer" {
		t.Errorf("id = %v, want integer/integer", id)
	}
	if !strings.Contains(id["description"].(string), "Primary Key") {
		t.Errorf("id description = %v, want PK note", id["description"])
	}
	title := props["title"].(map[string]any)
	if title["type"] != "string" || title["format"] != "text" {
		t.Errorf("title = %v, want string/text", title)
	}
	// required = non-nullable columns without a default, sorted.
	req := films["required"].([]any)
	got := make([]string, len(req))
	for i, c := range req {
		got[i] = c.(string)
	}
	if strings.Join(got, ",") != "id,title" {
		t.Errorf("required = %v, want [id title]", got)
	}
}

func TestForeignKeyNote(t *testing.T) {
	doc := decode(t, filmsModel(), nil, sqliteCaps(), Options{})
	props := doc["definitions"].(map[string]any)["films"].(map[string]any)["properties"].(map[string]any)
	fk := props["director_id"].(map[string]any)
	desc, _ := fk["description"].(string)
	if !strings.Contains(desc, "Foreign Key to `directors.id`") {
		t.Errorf("director_id description = %q, want FK note", desc)
	}
}

// TestOperatorAdvertisingHonorsCapabilities is the heart of the contract: a
// column parameter advertises match/imatch and the fts family on SQLite (regex
// and FTS5 present) but never the array/range operators (Unsupported).
func TestOperatorAdvertisingHonorsCapabilities(t *testing.T) {
	doc := decode(t, filmsModel(), nil, sqliteCaps(), Options{})
	params := doc["paths"].(map[string]any)["/films"].(map[string]any)["get"].(map[string]any)["parameters"].([]any)

	var colDesc string
	for _, p := range params {
		pm := p.(map[string]any)
		if pm["name"] == "title" {
			colDesc = pm["description"].(string)
		}
	}
	if colDesc == "" {
		t.Fatal("no title column parameter found")
	}
	for _, want := range []string{"eq", "match", "imatch", "fts", "plfts", "phfts", "wfts"} {
		if !containsToken(colDesc, want) {
			t.Errorf("operator %q should be advertised; desc = %q", want, colDesc)
		}
	}
	for _, gone := range []string{"cs", "cd", "ov", "sl", "sr", "adj"} {
		if containsToken(colDesc, gone) {
			t.Errorf("operator %q should be omitted on SQLite; desc = %q", gone, colDesc)
		}
	}
}

// TestRegexOmittedWhenUnsupported drops match/imatch when the backend grades
// regex Unsupported, proving the advertising is driven by the matrix.
func TestRegexOmittedWhenUnsupported(t *testing.T) {
	caps := sqliteCaps()
	caps.Regex = backend.Unsupported
	caps.FullText = backend.FTNone
	doc := decode(t, filmsModel(), nil, caps, Options{})
	params := doc["paths"].(map[string]any)["/films"].(map[string]any)["get"].(map[string]any)["parameters"].([]any)
	for _, p := range params {
		pm := p.(map[string]any)
		if pm["name"] != "title" {
			continue
		}
		desc := pm["description"].(string)
		for _, gone := range []string{"match", "imatch", "fts"} {
			if containsToken(desc, gone) {
				t.Errorf("operator %q should be omitted; desc = %q", gone, desc)
			}
		}
	}
}

func TestSecurityInactiveOmitsDefinitions(t *testing.T) {
	// PostgREST's default: with openapi-security-active off the document has
	// neither securityDefinitions nor a security requirement, even with JWT
	// auth configured on the server.
	doc := decode(t, filmsModel(), nil, sqliteCaps(), Options{})
	if _, ok := doc["securityDefinitions"]; ok {
		t.Error("securityDefinitions should be absent when security is inactive")
	}
	if _, ok := doc["security"]; ok {
		t.Error("security should be absent when security is inactive")
	}
}

func TestSecurityActiveEmitsJWTScheme(t *testing.T) {
	doc := decode(t, filmsModel(), nil, sqliteCaps(), Options{SecurityActive: true})
	sd, ok := doc["securityDefinitions"].(map[string]any)
	if !ok {
		t.Fatal("no securityDefinitions with security active")
	}
	jwt := sd["JWT"].(map[string]any)
	if jwt["type"] != "apiKey" || jwt["name"] != "Authorization" || jwt["in"] != "header" {
		t.Errorf("JWT scheme = %v", jwt)
	}
	if jwt["description"] != `Add the token prepending "Bearer " (without quotes) to it` {
		t.Errorf("JWT description = %v", jwt["description"])
	}
	// The requirement is document-level, the way v14 attaches it.
	sec, ok := doc["security"].([]any)
	if !ok || len(sec) != 1 {
		t.Fatalf("security = %v, want one document-level requirement", doc["security"])
	}
	if _, ok := sec[0].(map[string]any)["JWT"]; !ok {
		t.Errorf("security requirement = %v, want JWT", sec[0])
	}
	// Operations carry no per-operation security in v14.
	get := doc["paths"].(map[string]any)["/films"].(map[string]any)["get"].(map[string]any)
	if _, attached := get["security"]; attached {
		t.Error("security should not be attached per operation")
	}
}

func TestRPCPaths(t *testing.T) {
	reg := rpc.NewStaticRegistry([]*rpc.Function{
		{
			Name:       "add",
			Params:     []rpc.Param{{Name: "a", Type: "int4"}, {Name: "b", Type: "int4"}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "int4"},
			Volatility: rpc.Immutable,
		},
		{
			Name:       "log_event",
			Params:     []rpc.Param{{Name: "msg", Type: "text"}},
			Returns:    rpc.ReturnShape{Kind: rpc.ReturnScalar, Type: "void"},
			Volatility: rpc.Volatile,
		},
	})
	doc := decode(t, filmsModel(), reg, sqliteCaps(), Options{})
	paths := doc["paths"].(map[string]any)

	add, ok := paths["/rpc/add"].(map[string]any)
	if !ok {
		t.Fatal("missing /rpc/add")
	}
	if _, ok := add["get"]; !ok {
		t.Error("immutable function should be callable by GET")
	}
	if _, ok := add["post"]; !ok {
		t.Error("function should be callable by POST")
	}

	logEvent, ok := paths["/rpc/log_event"].(map[string]any)
	if !ok {
		t.Fatal("missing /rpc/log_event")
	}
	if _, ok := logEvent["get"]; ok {
		t.Error("volatile function should not be callable by GET")
	}
	if _, ok := logEvent["post"]; !ok {
		t.Error("volatile function should be callable by POST")
	}
}

// TestDeterministic checks the same inputs marshal byte-for-byte the same, so a
// cached document and a regenerated one match.
func TestDeterministic(t *testing.T) {
	a, _ := Generate(filmsModel(), nil, sqliteCaps(), Options{Host: "h"})
	b, _ := Generate(filmsModel(), nil, sqliteCaps(), Options{Host: "h"})
	if string(a) != string(b) {
		t.Error("generation is not deterministic")
	}
}

func TestOperatorTierResolution(t *testing.T) {
	caps := sqliteCaps()
	if operatorTier(ir.OpEq, caps) != backend.Native {
		t.Error("eq should be Native")
	}
	if operatorTier(ir.OpMatch, caps) != backend.Native {
		t.Error("match should follow Regex tier")
	}
	if operatorTier(ir.OpFTS, caps) != backend.BestEffort {
		t.Error("fts should be BestEffort on FTS5")
	}
	if operatorTier(ir.OpContains, caps).OK() {
		t.Error("cs should be Unsupported when ArrayRangeTypes is Unsupported")
	}
	// An explicit per-operator override wins over the feature class.
	caps.Operators = map[int]backend.Tier{int(ir.OpMatch): backend.Unsupported}
	if operatorTier(ir.OpMatch, caps).OK() {
		t.Error("explicit override should make match Unsupported")
	}
}

func BenchmarkGenerate(b *testing.B) {
	model := filmsModel()
	caps := sqliteCaps()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Generate(model, nil, caps, Options{Host: "localhost:3000"}); err != nil {
			b.Fatal(err)
		}
	}
}

// containsToken reports whether desc lists op as a comma-separated operator
// token, so "cs" does not match inside "docs" and "sr" does not match "string".
func containsToken(desc, op string) bool {
	_, list, ok := strings.Cut(desc, "Operators: ")
	if !ok {
		return false
	}
	list = strings.TrimSuffix(strings.TrimSpace(list), ".")
	return slices.Contains(strings.Split(list, ", "), op)
}
