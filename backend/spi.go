package backend

import (
	"context"
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
