// Package ir defines the engine-agnostic query intermediate representation and
// the request-parsing stage that produces it.
//
// The IR is the contract between the frontend and every backend (spec 03): the
// frontend parses an HTTP request into an IR tree with no knowledge of any
// engine, and each backend lowers that tree however its engine wants (SQL via a
// dialect, or a MongoDB pipeline). Parsing is pure syntax and identical on every
// backend; its errors are the PGRST1xx family. See spec 05-query-ir-and-planning.
package ir

import "github.com/tamnd/dbrest/schema"

// QueryKind is the operation a /<table> request performs.
type QueryKind uint8

const (
	Read QueryKind = iota
	Insert
	Update
	Upsert
	Delete
)

// CountKind selects the count strategy for the Content-Range header.
type CountKind uint8

const (
	CountNone CountKind = iota
	CountExact
	CountPlanned
	CountEstimated
)

// Ref names a relation or function. Name is what the client wrote; Schema is
// filled by the planner when it resolves the name against the model.
type Ref struct {
	Schema string
	Name   string
}

// Root is the top of a parsed request: exactly one of Query, Call, or Spec.
type Root struct {
	Query *Query
	Call  *Call
	Spec  *RootSpec // GET / -> OpenAPI
}

// Query is a /<table> request.
type Query struct {
	Kind      QueryKind
	Relation  Ref
	Select    []SelectItem
	Where     *Cond
	Order     []OrderTerm
	Limit     *int
	Offset    *int
	Embeds    []Embed
	Write     *WriteSpec // non-nil for Insert/Update/Upsert/Delete
	Singular  bool
	Count     CountKind
	Prefer    PreferSet
	FromRange bool // limit/offset came from the Range request header, not ?limit=
}

// Call is a /rpc/<fn> request.
type Call struct {
	Function Ref
	Args     map[string]Value
	ReadOnly bool
	Select   []SelectItem
	Where    *Cond
	Order    []OrderTerm
	Limit    *int
	Offset   *int
	Singular bool
	Count    CountKind
	Prefer   PreferSet
}

// RootSpec is a GET / request: render the OpenAPI document for a schema.
type RootSpec struct{ Schema string }

// SelectItem is one entry in the select list: a column, an aggregate, or a
// reference to an embed.
type SelectItem interface{ isSelect() }

// JSONStep records what the final hop of a JSON path returns.
type JSONStep uint8

const (
	JSONNone   JSONStep = iota // plain column
	JSONArrow                  // -> : returns json
	JSONArrow2                 // ->> : returns text
)

// Column is a base column with an optional JSON sub-path, cast, and alias.
//
// Path is the base column then JSON hops: col->a->>b is {"col","a","b"}.
type Column struct {
	Path  []string
	Last  JSONStep
	Cast  string
	Alias string
}

func (Column) isSelect() {}

// Name returns the output key for the column: its alias if set, else the last
// path element.
func (c Column) Name() string {
	if c.Alias != "" {
		return c.Alias
	}
	if len(c.Path) == 0 {
		return ""
	}
	return c.Path[len(c.Path)-1]
}

// AggFunc is an aggregate function.
type AggFunc uint8

const (
	AggCount AggFunc = iota
	AggSum
	AggAvg
	AggMin
	AggMax
)

// Aggregate is a column aggregate in the select list.
type Aggregate struct {
	Func  AggFunc
	Arg   *Column // nil for count(*)
	Cast  string
	Alias string
}

func (Aggregate) isSelect() {}

// EmbedRef points into Query.Embeds.
type EmbedRef struct{ Index int }

func (EmbedRef) isSelect() {}

// JoinKind is the join flavor for an embed.
type JoinKind uint8

const (
	JoinLeft  JoinKind = iota // default and !left
	JoinInner                 // !inner
)

// Cardinality decides whether an embedded value renders as an object or array.
type Cardinality uint8

const (
	CardUnknown Cardinality = iota
	CardToOne
	CardToMany
)

// Embed is a nested Query plus the resolved relationship and cardinality.
//
// Target is the relation as the client wrote it; OutKey is the response key the
// embedded value lands under (the alias when given, else the written name). Rel
// is filled by the planner once the name is resolved against the schema model;
// the compiler reads the cardinality and join columns from it. See spec 09.
type Embed struct {
	Cardinality Cardinality
	Join        JoinKind
	Spread      bool
	Hint        string
	Alias       string
	OutKey      string
	Target      Ref // the embedded relation as written; resolved at plan time
	Query       Query
	Rel         *schema.Relationship
}

// Cond is a node in the filter tree.
type Cond interface{ isCond() }

// And is a conjunction (and=(...)).
type And struct{ Kids []Cond }

func (And) isCond() {}

// Or is a disjunction (or=(...)).
type Or struct{ Kids []Cond }

func (Or) isCond() {}

// Not negates a logical sub-tree (not.and / not.or).
type Not struct{ Kid Cond }

func (Not) isCond() {}

// FTSVariant selects the full-text query grammar of an fts predicate, one per
// PostgREST operator. Parsing records the variant; each backend maps it onto its
// own full-text query language (spec 21).
type FTSVariant uint8

const (
	FTSPlain     FTSVariant = iota // fts: to_tsquery grammar (&, |, !, <->)
	FTSPlainText                   // plfts: plainto_tsquery, lexemes ANDed
	FTSPhrase                      // phfts: phraseto_tsquery, word order kept
	FTSWeb                         // wfts: websearch_to_tsquery, web-style string
)

// Compare is a single column-operator-value predicate.
type Compare struct {
	Path   []string
	Op     Op
	Value  Value
	Quant  Quant
	Negate bool
	// FTS is the full-text grammar when Op is OpFTS; Config is its optional
	// language argument (fts(english)), empty when absent. FullText is the covering
	// index the planner resolved for the predicate's column, nil when the schema
	// has none (the backend decides whether that is an error). See spec 21.
	FTS      FTSVariant
	Config   string
	FullText *schema.FullTextIndex
	// ColumnType is the canonical type of the column at Path[0], resolved by
	// the planner from the schema. The dialect uses it to decide whether an
	// engine-specific operator (e.g. json_each for array ops on SQLite) can
	// apply; it is empty when the column is unknown or for multi-step paths.
	ColumnType string
}

func (Compare) isCond() {}

// Quant is the (any)/(all) modifier on an operator.
type Quant uint8

const (
	QNone Quant = iota
	QAny
	QAll
)

// Op is the closed set of horizontal-filter operators (spec 02 operator table).
type Op int

const (
	OpEq Op = iota
	OpNeq
	OpGt
	OpGte
	OpLt
	OpLte
	OpLike
	OpILike
	OpMatch  // ~
	OpIMatch // ~*
	OpIn
	OpIs // null | true | false | unknown | not_null, carried in Value.Text
	OpIsDistinct
	OpFTS
	OpContains  // cs @>
	OpContained // cd <@
	OpOverlap   // ov &&
	OpRangeSL   // sl <<
	OpRangeSR   // sr >>
	OpRangeNXR  // nxr &<
	OpRangeNXL  // nxl &>
	OpRangeAdj  // adj -|-
)

// Value is a filter or argument value carried through the IR.
//
// For a horizontal filter it is the textual literal from the query string
// (the engine coerces it to the column type). List is populated for the `in`
// operator. JSON carries a decoded value for write payloads and POST RPC args.
type Value struct {
	Text string
	List []string
	JSON any
}

// OrderTerm is one entry in the order list.
type OrderTerm struct {
	Path       []string
	Desc       bool
	NullsFirst *bool // nil = PG default (NULLS LAST asc, NULLS FIRST desc)
}

// WriteSpec carries the mutation payload and options (spec 11).
type WriteSpec struct {
	Rows     []map[string]Value
	Set      map[string]Value
	Columns  []string
	Missing  MissingMode
	Conflict *Conflict
	Return   ReturnMode
	MaxRows  *int64
	Tx       TxMode
}

// MissingMode is the Prefer: missing= behavior for absent payload columns.
type MissingMode uint8

const (
	MissingDefault MissingMode = iota
	MissingNull
)

// Conflict describes an upsert conflict resolution.
type Conflict struct {
	Target     []string
	Resolution ConflictRes
}

// ConflictRes is merge-duplicates vs ignore-duplicates.
type ConflictRes uint8

const (
	ConflictMerge ConflictRes = iota
	ConflictIgnore
)

// ReturnMode is the Prefer: return= representation choice.
type ReturnMode uint8

const (
	ReturnMinimal ReturnMode = iota
	ReturnHeadersOnly
	ReturnRepresentation
)

// TxMode is the Prefer: tx= choice.
type TxMode uint8

const (
	TxAuto TxMode = iota
	TxCommit
	TxRollback
)
