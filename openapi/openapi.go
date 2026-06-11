// Package openapi generates the self-describing root PostgREST emits at GET /.
// The document is a Swagger 2.0 (OpenAPI) description of every exposed relation
// and function, built from the unified schema model and the RPC registry and
// served with the application/openapi+json media type.
//
// The generator is part of the frontend: it reads the already-built model and
// the backend's declared capabilities, never an engine catalog. The same model
// produces the same document on every backend, so the document's structure is
// portable; only the type precision tracks how exact the model is. See spec 19.
package openapi

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgtypes"
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

// MediaType is the content type PostgREST serves the root with.
const MediaType = "application/openapi+json"

// Options configures the emitted document's identity, server block, and
// security. The host/basePath/schemes are the externally visible address (the
// listen address, or the proxy URL once that configuration lands, spec 20).
type Options struct {
	Title    string   // document title; defaults to "dbrest"
	Version  string   // info.version; defaults to the compat target "14.0"
	Host     string   // host:port the API is reached at
	BasePath string   // mount path; defaults to "/"
	Schemes  []string // url schemes; defaults to ["http"]

	// ActiveSchema is the schema the document describes: the request's
	// profile-negotiated schema, so a multi-schema deployment serves one
	// document per schema and same-named relations never collide on a path key.
	ActiveSchema string

	// JWT advertises a bearer security scheme in securityDefinitions when true,
	// matching a server with JWT auth configured (spec 13).
	JWT bool
	// SecurityActive attaches the security requirement to every operation, the
	// PostgREST openapi-security-active setting (spec 20). With JWT defined but
	// this false, the scheme is described but not enforced, PostgREST's default.
	SecurityActive bool
}

func (o Options) withDefaults() Options {
	if o.Title == "" {
		o.Title = "dbrest"
	}
	if o.Version == "" {
		o.Version = "14.0"
	}
	if o.BasePath == "" {
		o.BasePath = "/"
	}
	if len(o.Schemes) == 0 {
		o.Schemes = []string{"http"}
	}
	return o
}

// Generate builds the OpenAPI document for the model and returns it as the JSON
// bytes the root serves. The capabilities decide which filter operators each
// column advertises, so the document never promises a feature the next request
// would reject with PGRST127.
func Generate(model *schema.Model, fns rpc.Registry, caps backend.Capabilities, opts Options) ([]byte, error) {
	return json.MarshalIndent(build(model, fns, caps, opts), "", "  ")
}

// build assembles the document struct. It is separated from Generate so a test
// can assert on the structure without reparsing JSON.
func build(model *schema.Model, fns rpc.Registry, caps backend.Capabilities, opts Options) *document {
	opts = opts.withDefaults()
	doc := &document{
		Swagger:     "2.0",
		Info:        info{Title: opts.Title, Version: opts.Version},
		Host:        opts.Host,
		BasePath:    opts.BasePath,
		Schemes:     opts.Schemes,
		Consumes:    []string{"application/json"},
		Produces:    []string{"application/json", "application/vnd.pgrst.object+json", "text/csv"},
		Paths:       map[string]*pathItem{},
		Definitions: map[string]*schemaObject{},
		Parameters:  reservedParameters(),
	}

	ops := advertisedTokens(caps)
	var security []map[string][]string
	if opts.JWT {
		doc.SecurityDefinitions = map[string]*securityScheme{
			"JWT": {Type: "apiKey", Name: "Authorization", In: "header"},
		}
		if opts.SecurityActive {
			security = []map[string][]string{{"JWT": {}}}
		}
	}

	for _, rel := range model.RelationsIn(opts.ActiveSchema) {
		doc.Paths["/"+rel.Name] = relationPath(rel, ops, security)
		doc.Definitions[rel.Name] = relationDefinition(rel)
	}
	if fns != nil {
		for _, fn := range fns.List() {
			doc.Paths["/rpc/"+fn.Name] = functionPath(fn, security)
		}
	}
	return doc
}

// relationPath emits the operations a relation supports. A base table gets the
// full read/write set; a view gets get only (updatable views land with the
// model flags that mark them so). Each operation lists the reserved parameters
// it honors plus one query parameter per column for horizontal filtering.
func relationPath(rel *schema.Relation, ops string, security []map[string][]string) *pathItem {
	filters := columnParams(rel, ops)
	get := &operation{
		Tags:       []string{rel.Name},
		Parameters: concat(refs("select", "order", "limit", "offset", "rangeHeader", "preferRead"), filters),
		Responses:  okResponses("200", "OK"),
		Security:   security,
	}
	p := &pathItem{Get: get}
	if rel.Kind == schema.KindTable {
		bodyRef := "#/definitions/" + rel.Name
		p.Post = &operation{
			Tags:       []string{rel.Name},
			Parameters: concat(refs("select", "columns", "on_conflict", "preferWrite"), []*parameter{bodyParam(rel.Name, bodyRef)}),
			Responses:  okResponses("201", "Created"),
			Security:   security,
		}
		p.Patch = &operation{
			Tags:       []string{rel.Name},
			Parameters: concat(refs("select", "columns", "preferWrite"), filters, []*parameter{bodyParam(rel.Name, bodyRef)}),
			Responses:  okResponses("204", "No Content"),
			Security:   security,
		}
		p.Delete = &operation{
			Tags:       []string{rel.Name},
			Parameters: concat(refs("preferWrite"), filters),
			Responses:  okResponses("204", "No Content"),
			Security:   security,
		}
	}
	return p
}

// relationDefinition builds the schema object for a relation from its columns.
// A property's type/format comes from the column's canonical type; required
// lists the non-nullable columns without a default; the primary key and foreign
// keys surface in the property descriptions the way PostgREST annotates them.
func relationDefinition(rel *schema.Relation) *schemaObject {
	def := &schemaObject{Type: "object", Properties: map[string]*propertySchema{}}
	pk := map[string]bool{}
	for _, c := range rel.PrimaryKey {
		pk[c] = true
	}
	for _, col := range rel.Columns {
		typ, format := swaggerType(col.Type)
		prop := &propertySchema{Type: typ, Format: format}
		prop.Description = columnNote(col.Name, pk, rel.ForeignKeys)
		def.Properties[col.Name] = prop
		if !col.Nullable && !col.HasDefault {
			def.Required = append(def.Required, col.Name)
		}
	}
	sort.Strings(def.Required)
	return def
}

// functionPath emits the /rpc/<fn> path. A read-only function (stable or
// immutable) is callable by GET with its arguments as query parameters and by
// POST with a body schema; a volatile function is POST only. See spec 12.
func functionPath(fn *rpc.Function, security []map[string][]string) *pathItem {
	p := &pathItem{}
	if fn.Volatility.ReadOnly() {
		p.Get = &operation{
			Tags:       []string{fn.Name},
			Parameters: functionQueryParams(fn),
			Responses:  okResponses("200", "OK"),
			Security:   security,
		}
	}
	p.Post = &operation{
		Tags:       []string{fn.Name},
		Parameters: []*parameter{{In: "body", Name: "args", Required: len(fn.Required()) > 0, Schema: functionBodySchema(fn)}},
		Responses:  okResponses("200", "OK"),
		Security:   security,
	}
	return p
}

func functionQueryParams(fn *rpc.Function) []*parameter {
	out := make([]*parameter, 0, len(fn.Params))
	for _, pm := range fn.Params {
		typ, format := swaggerType(pm.Type)
		out = append(out, &parameter{Name: pm.Name, In: "query", Required: !pm.Optional, Type: typ, Format: format})
	}
	return out
}

func functionBodySchema(fn *rpc.Function) *schemaObject {
	s := &schemaObject{Type: "object", Properties: map[string]*propertySchema{}}
	for _, pm := range fn.Params {
		typ, format := swaggerType(pm.Type)
		s.Properties[pm.Name] = &propertySchema{Type: typ, Format: format}
		if !pm.Optional {
			s.Required = append(s.Required, pm.Name)
		}
	}
	sort.Strings(s.Required)
	return s
}

// columnParams builds one query parameter per column, described with the
// operator grammar the backend can actually serve.
func columnParams(rel *schema.Relation, ops string) []*parameter {
	out := make([]*parameter, 0, len(rel.Columns))
	for _, col := range rel.Columns {
		_, format := swaggerType(col.Type)
		out = append(out, &parameter{
			Name:        col.Name,
			In:          "query",
			Type:        "string",
			Format:      format,
			Description: ops,
		})
	}
	return out
}

// advertisedTokens lists the filter operators a column may use, in a stable
// order, omitting any operator the backend grades Unsupported. The text is the
// shared description every column parameter carries (spec 19).
func advertisedTokens(caps backend.Capabilities) string {
	var toks []string
	for _, e := range operatorOrder {
		if operatorTier(e.op, caps).OK() {
			toks = append(toks, e.token)
		}
	}
	return "Horizontal filter. Format: col=op.value. Operators: " + strings.Join(toks, ", ") + "."
}

var operatorOrder = []struct {
	token string
	op    ir.Op
}{
	{"eq", ir.OpEq}, {"neq", ir.OpNeq},
	{"gt", ir.OpGt}, {"gte", ir.OpGte}, {"lt", ir.OpLt}, {"lte", ir.OpLte},
	{"like", ir.OpLike}, {"ilike", ir.OpILike},
	{"match", ir.OpMatch}, {"imatch", ir.OpIMatch},
	{"in", ir.OpIn}, {"is", ir.OpIs}, {"isdistinct", ir.OpIsDistinct},
	{"fts", ir.OpFTS}, {"plfts", ir.OpFTS}, {"phfts", ir.OpFTS}, {"wfts", ir.OpFTS},
	{"cs", ir.OpContains}, {"cd", ir.OpContained}, {"ov", ir.OpOverlap},
	{"sl", ir.OpRangeSL}, {"sr", ir.OpRangeSR},
	{"nxr", ir.OpRangeNXR}, {"nxl", ir.OpRangeNXL}, {"adj", ir.OpRangeAdj},
}

// operatorTier resolves the governing capability tier for one operator. An
// explicit per-operator override in the matrix wins; otherwise the operator
// inherits the tier of the feature class it belongs to (regex, full-text,
// array/range, is-distinct), and the plain comparison operators are Native
// everywhere.
func operatorTier(op ir.Op, caps backend.Capabilities) backend.Tier {
	if caps.Operators != nil {
		if t, ok := caps.Operators[int(op)]; ok {
			return t
		}
	}
	switch op {
	case ir.OpMatch, ir.OpIMatch:
		return caps.Regex
	case ir.OpFTS:
		if caps.FullText == backend.FTNone {
			return backend.Unsupported
		}
		if caps.FullText == backend.FTTSVector {
			return backend.Native
		}
		return backend.BestEffort
	case ir.OpIsDistinct:
		return caps.IsDistinctFrom
	case ir.OpContains, ir.OpContained, ir.OpOverlap,
		ir.OpRangeSL, ir.OpRangeSR, ir.OpRangeNXR, ir.OpRangeNXL, ir.OpRangeAdj:
		return caps.ArrayRangeTypes
	default:
		return backend.Native
	}
}

// swaggerType maps a canonical (or engine-aliased) type name to the Swagger
// type and format pair. The format is the model's own type name, so it carries
// exactly the precision the engine offered, no finer (spec 19, "limits").
func swaggerType(canonical string) (typ, format string) {
	if canonical == "" {
		return "string", ""
	}
	switch pgtypes.ClassOf(canonical) {
	case pgtypes.ClassInteger:
		return "integer", canonical
	case pgtypes.ClassFloat, pgtypes.ClassNumeric:
		return "number", canonical
	case pgtypes.ClassBool:
		return "boolean", canonical
	default:
		return "string", canonical
	}
}

// columnNote builds the PostgREST property annotation: a primary-key note, a
// foreign-key note, or both, joined the way PostgREST concatenates them. An
// unannotated column gets an empty description.
func columnNote(name string, pk map[string]bool, fks []*schema.ForeignKey) string {
	var notes []string
	if pk[name] {
		notes = append(notes, "Note:\nThis is a Primary Key.")
	}
	for _, fk := range fks {
		for i, c := range fk.Columns {
			if c != name {
				continue
			}
			ref := fk.RefRelation
			if i < len(fk.RefColumns) {
				ref += "." + fk.RefColumns[i]
			}
			notes = append(notes, "Note:\nThis is a Foreign Key to `"+ref+"`.")
		}
	}
	return strings.Join(notes, "\n")
}

// reservedParameters defines the shared parameters operations reference by
// $ref, mirroring the reserved query and header grammar (spec 02).
func reservedParameters() map[string]*parameter {
	return map[string]*parameter{
		"select":      {Name: "select", In: "query", Type: "string", Description: "Filtering and renaming columns"},
		"order":       {Name: "order", In: "query", Type: "string", Description: "Ordering"},
		"limit":       {Name: "limit", In: "query", Type: "integer", Description: "Limiting and pagination"},
		"offset":      {Name: "offset", In: "query", Type: "integer", Description: "Limiting and pagination"},
		"on_conflict": {Name: "on_conflict", In: "query", Type: "string", Description: "On conflict resolution columns"},
		"columns":     {Name: "columns", In: "query", Type: "string", Description: "Restricting and ordering inserted columns"},
		"preferRead":  {Name: "Prefer", In: "header", Type: "string", Description: "Preference: count, return"},
		"preferWrite": {Name: "Prefer", In: "header", Type: "string", Description: "Preference: return, resolution, missing"},
		"rangeHeader": {Name: "Range", In: "header", Type: "string", Description: "Limiting and pagination"},
	}
}

func bodyParam(name, ref string) *parameter {
	return &parameter{In: "body", Name: name, Schema: &schemaObject{Ref: ref}}
}

func refs(names ...string) []*parameter {
	out := make([]*parameter, len(names))
	for i, n := range names {
		out[i] = &parameter{Ref: "#/parameters/" + n}
	}
	return out
}

func concat(groups ...[]*parameter) []*parameter {
	var out []*parameter
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func okResponses(code, desc string) map[string]*response {
	return map[string]*response{code: {Description: desc}}
}
