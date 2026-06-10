package sqlite

import (
	"database/sql"
	"encoding/json"
	"io"
	"strings"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/reqctx"
)

// result adapts a *sql.Rows cursor to the backend.Result contract. SQLite does
// not assemble the response JSON itself on this path, so Body is nil and the
// renderer drives Rows. Engine-side JSON assembly (the Body path) arrives with
// the embedding subsystem.
type result struct {
	rows     *sql.Rows
	cols     []string
	controls *reqctx.ResponseControls
	count    int64
	hasCount bool
}

func (r *result) Body() io.Reader                            { return nil }
func (r *result) Rows() backend.RowStream                    { return &rowStream{rows: r.rows, cols: r.cols} }
func (r *result) Count() (int64, bool)                       { return r.count, r.hasCount }
func (r *result) Affected() (int64, bool)                    { return 0, false }
func (r *result) ResponseControls() *reqctx.ResponseControls { return r.controls }

// writeResult holds the buffered outcome of a mutation. A write runs inside a
// short transaction that must commit before the response is sent, so its rows
// are drained into memory rather than streamed from an open cursor. The buffer
// also lets the handler iterate twice (once for the Location primary key, once
// for the body) without re-running the statement.
type writeResult struct {
	cols     []string
	rows     [][]any
	affected int64
	hasAff   bool
	controls *reqctx.ResponseControls
}

func (r *writeResult) Body() io.Reader { return nil }
func (r *writeResult) Rows() backend.RowStream {
	return &bufStream{cols: r.cols, rows: r.rows, i: -1}
}
func (r *writeResult) Count() (int64, bool)                       { return 0, false }
func (r *writeResult) Affected() (int64, bool)                    { return r.affected, r.hasAff }
func (r *writeResult) ResponseControls() *reqctx.ResponseControls { return r.controls }

// bufStream replays buffered rows. Each call to Rows starts a fresh cursor at
// the first row, so a result can be iterated more than once.
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

// rowStream is a forward-only cursor over the result rows. Values decode each
// row into a []any the renderer maps to JSON by column name.
type rowStream struct {
	rows     *sql.Rows
	cols     []string
	colTypes []*sql.ColumnType // lazily populated on first call to Values
}

func (s *rowStream) Columns() []string { return s.cols }
func (s *rowStream) Next() bool        { return s.rows.Next() }
func (s *rowStream) Err() error        { return s.rows.Err() }
func (s *rowStream) Close() error      { return s.rows.Close() }

// Values scans the current row into Go values. SQLite returns int64, float64,
// string, []byte, or nil. Post-scan coercions:
//   - []byte → string so text columns render as JSON strings rather than base64.
//   - BOOLEAN/BOOL declared columns: int64 0/1 → false/true so JSON marshals
//     correctly as false/true rather than 0/1.
//   - JSON declared columns: string → json.RawMessage so the JSON encoder embeds
//     the value verbatim rather than quoting it as a string.
func (s *rowStream) Values() ([]any, error) {
	if s.colTypes == nil {
		ct, err := s.rows.ColumnTypes()
		if err == nil {
			s.colTypes = ct
		}
	}
	holders := make([]any, len(s.cols))
	ptrs := make([]any, len(s.cols))
	for i := range holders {
		ptrs[i] = &holders[i]
	}
	if err := s.rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	for i, v := range holders {
		if b, ok := v.([]byte); ok {
			v = string(b)
			holders[i] = v
		}
		if s.colTypes != nil && i < len(s.colTypes) {
			switch strings.ToUpper(s.colTypes[i].DatabaseTypeName()) {
			case "BOOLEAN", "BOOL":
				if n, ok := v.(int64); ok {
					holders[i] = n != 0
				}
			case "JSON":
				if str, ok := v.(string); ok {
					holders[i] = json.RawMessage(str)
				}
			}
		}
	}
	return holders, nil
}
