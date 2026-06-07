package sqlite

import (
	"database/sql"
	"io"

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

// rowStream is a forward-only cursor over the result rows. Values decode each
// row into a []any the renderer maps to JSON by column name.
type rowStream struct {
	rows *sql.Rows
	cols []string
}

func (s *rowStream) Columns() []string { return s.cols }
func (s *rowStream) Next() bool        { return s.rows.Next() }
func (s *rowStream) Err() error        { return s.rows.Err() }
func (s *rowStream) Close() error      { return s.rows.Close() }

// Values scans the current row into Go values. SQLite returns int64, float64,
// string, []byte, or nil; []byte is normalized to string so text columns render
// as JSON strings rather than base64.
func (s *rowStream) Values() ([]any, error) {
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
			holders[i] = string(b)
		}
	}
	return holders, nil
}
