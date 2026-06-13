package backend

import (
	"encoding/json"
	"maps"
	"strconv"

	"github.com/tamnd/dbrest/pgerr"
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
//
// A response.status that is not a valid HTTP status code is PGRST112, and a
// response.headers that is not the array-of-single-key-objects shape is PGRST111,
// matching the way PostgREST rejects a junk GUC rather than forwarding it. The
// error returns before the controls are applied, so a volatile function's
// transaction rolls back through the caller's deferred rollback.
func LiftResponseControls(cols []string, rows [][]any, controls *reqctx.ResponseControls) ([]string, [][]any, *pgerr.APIError) {
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
		return cols, rows, nil
	}

	if len(rows) > 0 && controls != nil {
		first := rows[0]
		if statusIdx >= 0 && statusIdx < len(first) {
			if v := first[statusIdx]; v != nil {
				code, ok := toStatus(v)
				if !ok || !validStatus(code) {
					return cols, rows, pgerr.ErrInvalidResponseStatus()
				}
				controls.SetStatus(code)
			}
		}
		if headersIdx >= 0 && headersIdx < len(first) {
			if v := first[headersIdx]; v != nil {
				hdrs, ok := toHeaders(v)
				if !ok {
					return cols, rows, pgerr.ErrInvalidResponseHeaders()
				}
				maps.Copy(controlHeaders(controls), hdrs)
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
	cols, rows = stripColumns(cols, rows, drop)
	return cols, rows, nil
}

// validStatus reports whether a status override is in the range an HTTP response
// can carry. net/http panics outside 100..999; PostgREST rejects anything that is
// not a real status code, so the tighter 100..599 range is used.
func validStatus(code int) bool { return code >= 100 && code <= 599 }

// controlHeaders returns the controls' header map, allocating it on first use so
// maps.Copy has a destination.
func controlHeaders(controls *reqctx.ResponseControls) map[string]string {
	if controls.Headers == nil {
		controls.Headers = map[string]string{}
	}
	return controls.Headers
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
// lone object is also accepted for convenience. ok is false when the value is
// present but not a JSON shape that can carry headers, the PGRST111 case.
func toHeaders(v any) (map[string]string, bool) {
	var raw []byte
	switch s := v.(type) {
	case string:
		raw = []byte(s)
	case json.RawMessage:
		raw = []byte(s)
	case []byte:
		raw = s
	default:
		return nil, false
	}
	out := map[string]string{}
	var list []map[string]string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, obj := range list {
			maps.Copy(out, obj)
		}
		return out, true
	}
	var obj map[string]string
	if err := json.Unmarshal(raw, &obj); err == nil {
		maps.Copy(out, obj)
		return out, true
	}
	return nil, false
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
