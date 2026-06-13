package backend

import (
	"encoding/json"
	"maps"
	"strconv"

	"github.com/tamnd/dbrest/reqctx"
)

// The reserved output columns a portable registry function projects to steer the
// response. A backend with a SQL-readable session store (PostgreSQL) lets a
// function call set_config('response.status', ...) and current_setting reads it
// back; an emulated backend has no setting a single SELECT can write, so a
// portable function carries the same intent as result columns named exactly like
// the GUCs. The column values use the same shapes the GUCs take: an integer
// status and a JSON array of single-key {name: value} header objects.
const (
	ColResponseStatus  = "response.status"
	ColResponseHeaders = "response.headers"
)

// HasResponseControlCols reports whether a result carries either reserved
// response-control column, so the caller can keep streaming the common case and
// only buffer when the controls must be lifted out.
func HasResponseControlCols(cols []string) bool {
	for _, c := range cols {
		if c == ColResponseStatus || c == ColResponseHeaders {
			return true
		}
	}
	return false
}

// LiftResponseControls folds a portable registry function's reserved
// response-control columns into the response controls and removes them from the
// body. The values are read from the first row (a function sets one status and
// one header set per request, matching the GUC model); a result with no rows
// leaves the controls untouched. The returned columns and rows have the reserved
// columns stripped so they never reach the rendered body. A result with neither
// reserved column is returned unchanged.
func LiftResponseControls(cols []string, rows [][]any, controls *reqctx.ResponseControls) ([]string, [][]any) {
	statusIdx, headersIdx := -1, -1
	for i, c := range cols {
		switch c {
		case ColResponseStatus:
			statusIdx = i
		case ColResponseHeaders:
			headersIdx = i
		}
	}
	if statusIdx < 0 && headersIdx < 0 {
		return cols, rows
	}

	if len(rows) > 0 && controls != nil {
		first := rows[0]
		if statusIdx >= 0 && statusIdx < len(first) {
			if code, ok := toStatus(first[statusIdx]); ok {
				controls.SetStatus(code)
			}
		}
		if headersIdx >= 0 && headersIdx < len(first) {
			for name, val := range toHeaders(first[headersIdx]) {
				controls.SetHeader(name, val)
			}
		}
	}

	drop := map[int]bool{}
	if statusIdx >= 0 {
		drop[statusIdx] = true
	}
	if headersIdx >= 0 {
		drop[headersIdx] = true
	}
	return stripColumns(cols, rows, drop)
}

// toStatus reads a status override from a reserved column value, accepting the
// integer the column most often holds as well as the float and string forms a
// driver may surface.
func toStatus(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i, true
		}
	case json.RawMessage:
		if i, err := strconv.Atoi(string(n)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// toHeaders reads response headers from a reserved column value. The value is the
// JSON the GUC convention uses: an array of single-key {name: value} objects. A
// lone object is also accepted for convenience.
func toHeaders(v any) map[string]string {
	var raw []byte
	switch s := v.(type) {
	case string:
		raw = []byte(s)
	case json.RawMessage:
		raw = []byte(s)
	case []byte:
		raw = s
	default:
		return nil
	}
	out := map[string]string{}
	var list []map[string]string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, obj := range list {
			maps.Copy(out, obj)
		}
		return out
	}
	var obj map[string]string
	if err := json.Unmarshal(raw, &obj); err == nil {
		maps.Copy(out, obj)
		return out
	}
	return nil
}

// stripColumns returns the columns and rows with the dropped indices removed,
// preserving order. It allocates new slices so the caller's buffers are left
// intact.
func stripColumns(cols []string, rows [][]any, drop map[int]bool) ([]string, [][]any) {
	keep := make([]int, 0, len(cols))
	for i := range cols {
		if !drop[i] {
			keep = append(keep, i)
		}
	}
	outCols := make([]string, len(keep))
	for i, idx := range keep {
		outCols[i] = cols[idx]
	}
	outRows := make([][]any, len(rows))
	for r, row := range rows {
		nr := make([]any, len(keep))
		for i, idx := range keep {
			if idx < len(row) {
				nr[i] = row[idx]
			}
		}
		outRows[r] = nr
	}
	return outCols, outRows
}
