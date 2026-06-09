package sqlserver

import (
	"database/sql"
	"encoding/json"
	"io"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/reqctx"
)

// result adapts an open *sql.Rows cursor to the backend.Result contract.
// Rows are streamed lazily; the transaction must stay open until Close is called.
type result struct {
	rows     *sql.Rows
	cols     []string
	jsonIdx  map[int]bool   // column indices carrying a JSON value (NVARCHAR with schema flag)
	timeIdx  map[int]string // column indices with time.Time → format string
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

// Values scans the current row with SQL Server-specific coercions:
//   - time.Time → DATE "2006-01-02" or DATETIME2 RFC3339 based on column type
//   - mssql.UniqueIdentifier → UUID string
//   - json-flagged NVARCHAR columns → json.RawMessage
func (s *rowStream) Values() ([]any, error) {
	holders := make([]any, len(s.r.cols))
	ptrs := make([]any, len(s.r.cols))
	for i := range holders {
		ptrs[i] = &holders[i]
	}
	if err := s.r.rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	coerce(holders, s.r.jsonIdx, s.r.timeIdx)
	return holders, nil
}

// drain reads all rows from a *sql.Rows cursor into a [][]any buffer.
func drain(rows *sql.Rows, cols []string, jsonIdx map[int]bool, timeIdx map[int]string) ([][]any, error) {
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
		coerce(holders, jsonIdx, timeIdx)
		out = append(out, holders)
	}
	return out, rows.Err()
}

// coerce applies SQL Server-specific type coercions to a scanned row.
func coerce(holders []any, jsonIdx map[int]bool, timeIdx map[int]string) {
	for i, v := range holders {
		switch t := v.(type) {
		case time.Time:
			var fmt string
			if timeIdx != nil {
				fmt = timeIdx[i]
			}
			if fmt == "" {
				fmt = defaultTimeFormat(t)
			}
			holders[i] = t.UTC().Format(fmt)
		case mssql.UniqueIdentifier:
			holders[i] = t.String()
		case []byte:
			if jsonIdx[i] || looksLikeJSON(t) {
				holders[i] = json.RawMessage(t)
			} else {
				holders[i] = string(t)
			}
		case string:
			if jsonIdx[i] || looksLikeJSON([]byte(t)) {
				holders[i] = json.RawMessage(t)
			}
		}
	}
}

// looksLikeJSON returns true when the bytes start with [ or {, indicating a
// JSON array or object stored as a string (e.g. in NVARCHAR(MAX) columns).
// This is a fast heuristic; full validation happens at the JSON encoder.
func looksLikeJSON(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	return b[0] == '[' || b[0] == '{'
}

// defaultTimeFormat returns a format string based on whether the time has a
// non-zero time component.
func defaultTimeFormat(t time.Time) string {
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return "2006-01-02"
	}
	return "2006-01-02T15:04:05Z07:00"
}

// buildColMaps inspects the column types of a *sql.Rows to build the jsonIdx
// and timeIdx maps used by coerce. jsonCols identifies NVARCHAR columns that
// should be treated as JSON (from schema introspection or explicit flag).
func buildColMaps(rows *sql.Rows, jsonCols map[string]bool) (jsonIdx map[int]bool, timeIdx map[int]string) {
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil
	}
	for i, ct := range cts {
		switch ct.DatabaseTypeName() {
		case "DATE":
			if timeIdx == nil {
				timeIdx = make(map[int]string)
			}
			timeIdx[i] = "2006-01-02"
		case "DATETIME", "DATETIME2", "SMALLDATETIME", "DATETIMEOFFSET":
			if timeIdx == nil {
				timeIdx = make(map[int]string)
			}
			timeIdx[i] = "2006-01-02T15:04:05Z07:00"
		case "NVARCHAR", "VARCHAR":
			if jsonCols[ct.Name()] {
				if jsonIdx == nil {
					jsonIdx = make(map[int]bool)
				}
				jsonIdx[i] = true
			}
		case "JSON":
			if jsonIdx == nil {
				jsonIdx = make(map[int]bool)
			}
			jsonIdx[i] = true
		}
	}
	return jsonIdx, timeIdx
}
