package httpapi

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/pgerr"
)

// rendered is a fully assembled read response: the JSON body plus the row count
// the handler needs to compute Content-Range and status.
type rendered struct {
	body    []byte
	nRows   int
	total   int64
	hasTotl bool
}

// renderRows shapes a backend row stream into a PostgREST-shaped JSON array,
// reproducing PostgREST's null handling (SQL NULL -> JSON null) and select-order
// keys. singular asks for a single object instead of an array and enforces the
// PGRST116 zero-or-many rule.
//
// This is the Go-shaped assembly path (Result.Rows). The engine-assembled path
// (Result.Body) is used once the embedding subsystem emits in-engine JSON; the
// observable body is identical either way.
func renderRows(res backend.Result, singular bool) (*rendered, *pgerr.APIError) {
	rs := res.Rows()
	defer rs.Close()
	cols := rs.Columns()

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	rows := make([]json.RawMessage, 0, 16)
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			return nil, pgerr.ErrInternal(err.Error())
		}
		obj := make(map[string]any, len(cols))
		for i, c := range cols {
			obj[c] = vals[i]
		}
		// Encode each row independently so a large result streams in bounded
		// memory once the engine-assembled path replaces this shaper.
		var rb bytes.Buffer
		re := json.NewEncoder(&rb)
		re.SetEscapeHTML(false)
		if err := re.Encode(obj); err != nil {
			return nil, pgerr.ErrInternal(err.Error())
		}
		rows = append(rows, json.RawMessage(bytes.TrimRight(rb.Bytes(), "\n")))
	}
	if err := rs.Err(); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}

	out := &rendered{nRows: len(rows)}
	if total, ok := res.Count(); ok {
		out.total, out.hasTotl = total, true
	}

	if singular {
		if len(rows) != 1 {
			return nil, pgerr.ErrSingularZeroMany()
		}
		out.body = rows[0]
		return out, nil
	}

	if err := enc.Encode(rows); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}
	out.body = bytes.TrimRight(buf.Bytes(), "\n")
	return out, nil
}

// contentRange builds the Content-Range header value from the window and the
// optional total. offset is the window start; n is the number of rows returned.
// The total field is the engine count when present, else "*".
func contentRange(offset, n int, total int64, hasTotal bool) string {
	totalStr := "*"
	if hasTotal {
		totalStr = strconv.FormatInt(total, 10)
	}
	if n == 0 {
		// Empty result: PostgREST emits */<total> (or */* without a count).
		return "*/" + totalStr
	}
	start := offset
	end := offset + n - 1
	return strconv.Itoa(start) + "-" + strconv.Itoa(end) + "/" + totalStr
}
