package postgres

import (
	"context"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/reqctx"
)

// streamResult adapts an open pgx cursor to the backend.Result contract for a
// read. The role and request GUCs are transaction-local, so the cursor must stay
// inside its transaction while it streams; the result therefore owns the
// transaction and commits it when the renderer closes the stream. A read commits
// rather than rolls back so that anything a stable function did in passing (a
// session setting, say) is not needlessly discarded; a read changes no data, so
// the choice is immaterial to durability.
type streamResult struct {
	ctx      context.Context
	tx       pgx.Tx
	rows     pgx.Rows
	cols     []string
	controls *reqctx.ResponseControls
	count    int64
	hasCount bool
}

func (r *streamResult) Body() io.Reader { return nil }
func (r *streamResult) Rows() backend.RowStream {
	return &streamRows{ctx: r.ctx, tx: r.tx, rows: r.rows, cols: r.cols}
}
func (r *streamResult) Count() (int64, bool)                       { return r.count, r.hasCount }
func (r *streamResult) Affected() (int64, bool)                    { return 0, false }
func (r *streamResult) ResponseControls() *reqctx.ResponseControls { return r.controls }

// streamRows is a forward-only cursor over a read's rows. Close releases the
// cursor and commits the owning transaction; the transaction is committed once,
// on the renderer's deferred Close.
type streamRows struct {
	ctx  context.Context
	tx   pgx.Tx
	rows pgx.Rows
	cols []string
}

func (s *streamRows) Columns() []string { return s.cols }
func (s *streamRows) Next() bool        { return s.rows.Next() }
func (s *streamRows) Err() error        { return s.rows.Err() }

// Values returns the current row decoded to Go types. pgx maps PostgreSQL types
// to Go values (int for the integer types, float64, bool, string, time.Time,
// []byte for json/jsonb and bytea, and slices for arrays); normalizeValues folds
// the byte forms to the shapes the renderer expects.
func (s *streamRows) Values() ([]any, error) {
	vals, err := s.rows.Values()
	if err != nil {
		return nil, err
	}
	return normalizeValues(vals), nil
}

// Close releases the cursor and commits the transaction that scoped the role and
// request GUCs. Committing a read-only transaction is cheap and never fails on a
// healthy connection; a commit error is returned so the renderer can surface it.
func (s *streamRows) Close() error {
	s.rows.Close()
	if err := s.rows.Err(); err != nil {
		_ = s.tx.Rollback(s.ctx)
		return err
	}
	return s.tx.Commit(s.ctx)
}

// bufResult holds the buffered outcome of a write or a function call. A write
// runs inside a transaction that must commit (or roll back, under tx=rollback)
// before the response is sent, and a function call's response headers and status
// are read back from GUCs after the call returns, so both buffer their rows into
// memory rather than streaming from an open cursor. The buffer also lets the
// handler iterate twice (once for the Location primary key, once for the body)
// without re-running the statement.
type bufResult struct {
	cols     []string
	rows     [][]any
	affected int64
	hasAff   bool
	count    int64
	hasCount bool
	controls *reqctx.ResponseControls
}

func (r *bufResult) Body() io.Reader { return nil }
func (r *bufResult) Rows() backend.RowStream {
	return &bufStream{cols: r.cols, rows: r.rows, i: -1}
}
func (r *bufResult) Count() (int64, bool)                       { return r.count, r.hasCount }
func (r *bufResult) Affected() (int64, bool)                    { return r.affected, r.hasAff }
func (r *bufResult) ResponseControls() *reqctx.ResponseControls { return r.controls }

// bufStream replays buffered rows. Each call to Rows starts a fresh cursor at the
// first row, so a result can be iterated more than once.
type bufStream struct {
	cols []string
	rows [][]any
	i    int
}

func (s *bufStream) Columns() []string      { return s.cols }
func (s *bufStream) Next() bool             { s.i++; return s.i < len(s.rows) }
func (s *bufStream) Values() ([]any, error) { return s.rows[s.i], nil }
func (s *bufStream) Err() error             { return nil }
func (s *bufStream) Close() error           { return nil }

// normalizeValues adjusts pgx's decoded values to the shapes the renderer maps to
// JSON. json and jsonb arrive as raw bytes; they are turned into strings so the
// renderer's raw-JSON columns pass them through verbatim rather than base64. A
// bytea value also arrives as bytes, but its column is not a raw-JSON column, so
// it renders as a string like the other backends. Every other value is left as
// pgx decoded it.
func normalizeValues(vals []any) []any {
	for i, v := range vals {
		if b, ok := v.([]byte); ok {
			vals[i] = string(b)
		}
	}
	return vals
}
