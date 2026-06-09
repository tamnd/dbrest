package mysql

import (
	"database/sql"
	"encoding/json"
	"io"
	"time"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/reqctx"
)

// result adapts an open *sql.Rows cursor to the backend.Result contract.
// Rows are streamed lazily; the transaction must stay open until Close is called.
type result struct {
	rows     *sql.Rows
	cols     []string
	jsonIdx  map[int]bool   // column indices carrying a JSON value
	boolIdx  map[int]bool   // column indices that need int8/uint8 → bool coercion
	timeIdx  map[int]string // column indices with time.Time values → format string
	controls *reqctx.ResponseControls
	count    int64
	hasCount bool
}

func (r *result) Body() io.Reader                            { return nil }
func (r *result) Rows() backend.RowStream                    { return &rowStream{r: r} }
func (r *result) Count() (int64, bool)                       { return r.count, r.hasCount }
func (r *result) Affected() (int64, bool)                    { return 0, false }
func (r *result) ResponseControls() *reqctx.ResponseControls { return r.controls }

// writeResult holds the buffered outcome of a mutation (INSERT/UPDATE/DELETE).
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

// bufStream replays buffered rows.
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

// rowStream is a forward-only cursor over a live *sql.Rows result set.
type rowStream struct {
	r *result
}

func (s *rowStream) Columns() []string { return s.r.cols }
func (s *rowStream) Next() bool        { return s.r.rows.Next() }
func (s *rowStream) Err() error        { return s.r.rows.Err() }
func (s *rowStream) Close() error      { return s.r.rows.Close() }

// Values scans the current row into Go values with MySQL-specific coercions:
//   - JSON columns: []byte → json.RawMessage (inline JSON, not base64)
//   - DATE columns: time.Time → "2006-01-02" string
//   - DATETIME/TIMESTAMP: time.Time → RFC3339 string
//   - BOOL/TINYINT(1): int8/uint8 → bool (via boolIdx from schema introspection)
func (s *rowStream) Values() ([]any, error) {
	holders := make([]any, len(s.r.cols))
	ptrs := make([]any, len(s.r.cols))
	for i := range holders {
		ptrs[i] = &holders[i]
	}
	if err := s.r.rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	coerce(holders, s.r.jsonIdx, s.r.boolIdx, s.r.timeIdx)
	return holders, nil
}

// drain reads all rows from a *sql.Rows cursor into a [][]any buffer, applying
// JSON, bool, and time coercions. Used for writes that buffer results before commit.
func drain(rows *sql.Rows, cols []string, jsonIdx, boolIdx map[int]bool) ([][]any, error) {
	var out [][]any
	for rows.Next() {
		holders := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range holders {
			ptrs[i] = &holders[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		coerce(holders, jsonIdx, boolIdx, nil)
		out = append(out, holders)
	}
	return out, rows.Err()
}

// coerce applies MySQL-specific type coercions to a scanned row.
// jsonIdx marks JSON column indices; boolIdx marks BOOL/TINYINT(1) columns;
// timeIdx maps index → format string. If timeIdx is nil, time.Time values
// are formatted using defaultTimeFormat.
//
// TINYINT values arrive as int8/uint8 (binary protocol) or as int64/[]byte
// (text protocol, no query args). All four are handled for boolIdx columns.
func coerce(holders []any, jsonIdx, boolIdx map[int]bool, timeIdx map[int]string) {
	for i, v := range holders {
		switch t := v.(type) {
		case []byte:
			switch {
			case jsonIdx[i]:
				holders[i] = json.RawMessage(t)
			case boolIdx[i]:
				holders[i] = len(t) > 0 && t[0] == '1'
			default:
				holders[i] = string(t)
			}
		case time.Time:
			var fmt string
			if timeIdx != nil {
				fmt = timeIdx[i]
			}
			if fmt == "" {
				fmt = defaultTimeFormat(t)
			}
			holders[i] = t.Format(fmt)
		case int8:
			if boolIdx[i] {
				holders[i] = t != 0
			}
		case uint8:
			if boolIdx[i] {
				holders[i] = t != 0
			}
		case int64:
			if boolIdx[i] {
				holders[i] = t != 0
			}
		case uint64:
			if boolIdx[i] {
				holders[i] = t != 0
			}
		}
	}
}

// defaultTimeFormat picks a format based on the time value's time-of-day:
// if the time component is all zeroes the value is a bare date.
func defaultTimeFormat(t time.Time) string {
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return "2006-01-02"
	}
	return "2006-01-02T15:04:05Z07:00"
}

// buildColMaps inspects the column types of a *sql.Rows to build the jsonIdx,
// boolIdx, and timeIdx maps used by the coerce functions. boolCols contains the
// column names that should be treated as boolean (from schema introspection).
// Must be called before iterating rows.
func buildColMaps(rows *sql.Rows, boolCols map[string]bool) (jsonIdx, boolIdx map[int]bool, timeIdx map[int]string) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, nil
	}
	for i, ct := range cts {
		switch ct.DatabaseTypeName() {
		case "JSON":
			if jsonIdx == nil {
				jsonIdx = make(map[int]bool)
			}
			jsonIdx[i] = true
		case "DATE":
			if timeIdx == nil {
				timeIdx = make(map[int]string)
			}
			timeIdx[i] = "2006-01-02"
		case "DATETIME", "TIMESTAMP":
			if timeIdx == nil {
				timeIdx = make(map[int]string)
			}
			timeIdx[i] = "2006-01-02T15:04:05Z07:00"
		case "TINYINT":
			if boolCols[ct.Name()] {
				if boolIdx == nil {
					boolIdx = make(map[int]bool)
				}
				boolIdx[i] = true
			}
		}
	}
	return jsonIdx, boolIdx, timeIdx
}
