// Package backend defines the service-provider interface every engine
// implements, plus the Capabilities type the planner consults. The frontend
// talks only to this package, never to a concrete backend, so adding a database
// means implementing this interface once. See spec 03/04.
package backend

// Tier is the four-tier capability grade for a feature (spec 04).
type Tier uint8

const (
	// Unsupported: no faithful equivalent; a request needing it returns PGRST127.
	Unsupported Tier = iota
	// BestEffort: an approximation with a documented divergence.
	BestEffort
	// Emulated: dbrest builds an equivalent with the same observable behavior.
	Emulated
	// Native: the engine does what PostgreSQL does; lower and pass through.
	Native
)

// String renders the tier as its matrix letter (N/E/B/U).
func (t Tier) String() string {
	switch t {
	case Native:
		return "N"
	case Emulated:
		return "E"
	case BestEffort:
		return "B"
	default:
		return "U"
	}
}

// OK reports whether the tier permits serving the feature at all (anything but
// Unsupported). The planner uses it to decide whether to emit PGRST127.
func (t Tier) OK() bool { return t != Unsupported }

// TxTier grades transaction support.
type TxTier uint8

const (
	TxNone TxTier = iota
	TxLimited
	TxFull
)

// FTKind names the full-text engine flavor.
type FTKind uint8

const (
	FTNone FTKind = iota
	FTTSVector
	FTSQLite5
	FTMySQL
	FTMSSQL
	FTMongo
)

// SchemaKind names how the backend exposes schemas.
type SchemaKind uint8

const (
	SchemaNone SchemaKind = iota
	SchemaNative
	SchemaAttached
	SchemaPrefixed
)

// EmbedKind names how the backend performs resource embedding.
type EmbedKind uint8

const (
	EmbedNone EmbedKind = iota
	EmbedJoin
	EmbedPipeline
)

// Capabilities describes what a backend can do, per feature class. It is the
// planner's only window into a backend; the frontend never special-cases an
// engine by name. Computed once at Open from the connected server version, so a
// version-gated feature resolves to the right tier for the actual server.
type Capabilities struct {
	Returning            Tier
	Upsert               Tier
	UpsertConflictTarget bool
	NullsOrdering        Tier
	JSONAssembly         Tier
	IsDistinctFrom       Tier
	Transactions         TxTier
	NativeRoles          bool
	NativeRLS            bool
	SessionContext       Tier
	NativeRPC            bool
	Regex                Tier
	FullText             FTKind
	ArrayRangeTypes      Tier
	Schemas              SchemaKind
	Aggregates           Tier
	Embedding            EmbedKind
	CountPlanned         Tier

	// Operators grades the horizontal-filter operators that vary by engine.
	// Operators absent from the map are assumed Native (the common case where
	// eq/gt/in/... pass straight through). The planner reads this to decide
	// degradation per operator. Keyed by ir.Op rendered as int.
	Operators map[int]Tier
}

// Operator returns the tier for operator op (as int(ir.Op)), defaulting to
// Native when the backend did not override it.
func (c Capabilities) Operator(op int) Tier {
	if c.Operators == nil {
		return Native
	}
	if t, ok := c.Operators[op]; ok {
		return t
	}
	return Native
}
