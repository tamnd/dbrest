package backend

import (
	"context"
	"fmt"
	"io"

	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

// Backend is the contract every engine implements. The frontend talks only to
// this interface (spec 03).
//
// Note on scope: this interface grows as subsystems land. The read path needs
// Capabilities/Introspect/Execute/MapError/Close; Functions() returns the RPC
// registry for the /rpc/<fn> endpoint. See the implementation spec.
type Backend interface {
	// Capabilities describes what this backend can do, per feature class.
	// Static for a given backend+engine version; computed once at Open.
	Capabilities() Capabilities

	// Introspect builds the unified schema model from the engine's catalogs
	// (or, for a schemaless store, from declared config plus sampling).
	Introspect(ctx context.Context) (*schema.Model, error)

	// Functions returns the callable functions exposed at /rpc/<fn>: native
	// discovery from the engine catalog, the portable registry, or both behind
	// one interface. A backend with none returns an empty registry.
	Functions() rpc.Registry

	// Execute lowers the plan to concrete engine operations, runs them inside a
	// per-request transaction whose mode is given by the plan, and returns a
	// streamable result.
	Execute(ctx context.Context, plan *ir.Plan, rc *reqctx.Context) (Result, error)

	// MapError turns an engine-native error into the unified API error envelope.
	// Returns nil if the error is not engine-recognized (treated as internal).
	MapError(err error) *pgerr.APIError

	// Close releases the pool.
	Close() error
}

// SchemaFunctioner is an optional capability of a NativeRPC backend that
// introspects its own functions: it exposes them as a registry per exposed schema,
// the function half of the schema cache. The frontend uses it to resolve native
// overloads, raise PGRST202/PGRST203, and partition GET arguments from result
// filters through the same planner the portable registry uses, instead of building
// a minimal plan and deferring everything to the engine. A backend that does not
// implement it keeps the verb-derived minimal plan. PostgreSQL implements it.
type SchemaFunctioner interface {
	SchemaFunctions(schema string) rpc.Registry
}

// Result is the streaming response abstraction. A backend returns either an
// assembled Body (the engine built the JSON) or a RowStream the renderer shapes
// in Go. Which one is recorded by the JSONAssembly capability (spec 03).
type Result interface {
	// Body is engine-assembled bytes, or nil if Rows is used.
	Body() io.Reader
	// Rows is a row-at-a-time stream, or nil if Body is used.
	Rows() RowStream
	// Count is the total for Content-Range, when requested.
	Count() (int64, bool)
	// Affected is rows affected, for writes and max-affected.
	Affected() (int64, bool)
	// ResponseControls are status/header overrides read back after Execute.
	ResponseControls() *reqctx.ResponseControls
}

// PlanFormat is the output format an Accept: application/vnd.pgrst.plan request
// asks for. PostgREST defaults to text (bare type and the +text suffix); +json
// asks for the machine-readable form.
type PlanFormat uint8

const (
	PlanText PlanFormat = iota // default: EXPLAIN text output
	PlanJSON                   // +json suffix: EXPLAIN (FORMAT JSON)
)

// PlanOptions carries the parsed parameters of a plan Accept header. Format
// selects text vs json; For is the media type the plan is computed for (the
// for="<media>" parameter, informational on the wire and echoed back); the
// booleans are the options= flags PostgREST forwards to EXPLAIN.
type PlanOptions struct {
	Format   PlanFormat
	For      string
	Analyze  bool
	Verbose  bool
	Settings bool
	Buffers  bool
	Wal      bool
}

// Explainer is an optional backend capability for the application/vnd.pgrst.plan
// Accept type. Backends that support EXPLAIN implement this interface; the
// frontend type-asserts to it and 406s when it is absent. The three methods
// mirror the three execution paths so a plan can be requested for a read, a
// write, or an RPC call. Each returns the engine's EXPLAIN output already
// formatted per opts.Format (text bytes or a JSON document).
type Explainer interface {
	ExplainRead(ctx context.Context, p *ir.Plan, rc *reqctx.Context, opts PlanOptions) ([]byte, error)
	ExplainWrite(ctx context.Context, p *ir.Plan, rc *reqctx.Context, opts PlanOptions) ([]byte, error)
	ExplainCall(ctx context.Context, p *ir.Plan, rc *reqctx.Context, opts PlanOptions) ([]byte, error)
}

// RowStream is a forward-only cursor over result rows. The renderer drives it to
// assemble the response body when the backend does not assemble JSON itself.
type RowStream interface {
	// Columns returns the output column names, in order.
	Columns() []string
	// Next advances to the next row, returning false at end or on error (check Err).
	Next() bool
	// Values returns the current row's values, decoded to Go types.
	Values() ([]any, error)
	// Err returns the first error encountered during iteration.
	Err() error
	// Close releases the cursor.
	Close() error
}

// EnforceMaxAffected is the Prefer: max-affected contract every write backend
// shares. WriteSpec.MaxRows is set only under handling=strict (ir.ParsePrefer
// clears it under lenient), so a non-nil bound always means "enforce". When the
// mutation affected more rows than the bound, it returns PGRST124; the backend
// then returns before commit and its deferred rollback discards the over-broad
// write. It returns nil when no bound is set, the affected count is unknown, or
// the count is within the bound. Callers must invoke it after the affected count
// is known and before commit.
func EnforceMaxAffected(w *ir.WriteSpec, affected int64, hasAffected bool) *pgerr.APIError {
	if w == nil || w.MaxRows == nil || !hasAffected {
		return nil
	}
	if affected > *w.MaxRows {
		return pgerr.ErrMaxAffected(affected)
	}
	return nil
}

// EnforceSingularWrite is the single-object guarantee a write makes when the
// client negotiated application/vnd.pgrst.object+json (q.Singular): the mutation
// must affect exactly one row. A zero-or-many result is PGRST116. Callers invoke
// it after the affected count is known and before commit, so the failure rolls
// the mutation back through the backend's deferred rollback rather than the
// renderer noticing the wrong count after the write is already durable
// (PostgREST's condemn discipline). The renderer keeps the same check for reads,
// where there is no transaction to roll back. It is a no-op for a non-singular
// request or when the count is unknown.
func EnforceSingularWrite(singular bool, affected int64, hasAffected bool) *pgerr.APIError {
	if !singular || !hasAffected {
		return nil
	}
	if affected != 1 {
		return pgerr.ErrSingularZeroMany().
			WithDetails(fmt.Sprintf("The result contains %d rows", affected))
	}
	return nil
}
