package httpapi

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/rpc"
)

// rendered is a fully assembled response: the body, the negotiated Content-Type,
// and the row count the handler needs to compute Content-Range and status.
type rendered struct {
	body        []byte
	contentType string
	nRows       int
	total       int64
	hasTotl     bool
}

// renderFor encodes a backend result into the negotiated media type. The JSON
// family rides renderRows (and the singular PGRST116 rule); CSV and the scalar
// types shape the row stream directly. See spec 17-content-negotiation.
func renderFor(media string, res backend.Result, rawCols map[string]bool) (*rendered, *pgerr.APIError) {
	switch media {
	case mediaJSON, mediaArray:
		out, err := renderRows(res, false, rawCols, false)
		if err != nil {
			return nil, err
		}
		out.contentType = "application/json; charset=utf-8"
		return out, nil
	case mediaArrayStripped:
		out, err := renderRows(res, false, rawCols, true)
		if err != nil {
			return nil, err
		}
		out.contentType = "application/vnd.pgrst.array+json; nulls=stripped; charset=utf-8"
		return out, nil
	case mediaObject:
		out, err := renderRows(res, true, rawCols, false)
		if err != nil {
			return nil, err
		}
		out.contentType = singularMediaType + "; charset=utf-8"
		return out, nil
	case mediaObjectStripped:
		out, err := renderRows(res, true, rawCols, true)
		if err != nil {
			return nil, err
		}
		out.contentType = "application/vnd.pgrst.object+json; nulls=stripped; charset=utf-8"
		return out, nil
	case mediaCSV:
		return renderCSV(res)
	case mediaOctet:
		return renderScalar(res, false)
	case mediaText:
		return renderScalar(res, true)
	default:
		return nil, pgerr.ErrInternal("no renderer for media type " + media)
	}
}

// renderCall shapes an RPC result by the function's declared return kind. A
// table return renders exactly like a read (objects in the JSON family, or CSV /
// scalar media). A scalar return is the bare value; a setof-scalar return is a
// JSON array of bare values. The object media type asks for a single value and
// enforces the zero-or-many rule, so a setof function with one row can satisfy a
// singular request. fnName is the bare function name; it is used for native-RPC
// heuristic detection when fn is nil.
func renderCall(media string, res backend.Result, fn *rpc.Function, fnName string) (*rendered, *pgerr.APIError) {
	if fn == nil {
		// Native RPC: detect scalar vs table by inspecting column names.
		// res.Rows().Columns() does not advance the cursor; the stream remains
		// fully readable for the render path below.
		cols := res.Rows().Columns()
		if len(cols) == 1 && cols[0] == fnName {
			fn = &rpc.Function{Returns: rpc.ReturnShape{Kind: rpc.ReturnScalar}}
		} else {
			return renderFor(media, res, nil)
		}
	} else if fn.Returns.Kind == rpc.ReturnTable {
		return renderFor(media, res, nil)
	} else if fn.Returns.Kind == rpc.ReturnVoid {
		return renderVoid(res)
	}
	switch media {
	case mediaCSV:
		return renderCSV(res)
	case mediaOctet:
		return renderScalar(res, false)
	case mediaText:
		return renderScalar(res, true)
	}

	rs := res.Rows()
	defer rs.Close()
	cols := rs.Columns()

	var vals []any
	for rs.Next() {
		row, err := rs.Values()
		if err != nil {
			return nil, pgerr.ErrInternal(err.Error())
		}
		if len(cols) == 0 {
			vals = append(vals, nil)
			continue
		}
		// A scalar function projects one column; if a registry declares scalar
		// over a wider statement, the first column is the value.
		vals = append(vals, rawJSONValue(row[0], fn.Returns.Type))
	}
	if err := rs.Err(); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}

	out := &rendered{nRows: len(vals)}
	if total, ok := res.Count(); ok {
		out.total, out.hasTotl = total, true
	}

	if singularMedia(media) {
		if len(vals) != 1 {
			return nil, pgerr.ErrSingularZeroMany().
				WithDetails(fmt.Sprintf("The result contains %d rows", len(vals)))
		}
		body, aerr := marshalCall(vals[0])
		if aerr != nil {
			return nil, aerr
		}
		out.body, out.contentType = body, singularMediaType+"; charset=utf-8"
		return out, nil
	}

	out.contentType = "application/json; charset=utf-8"
	if fn.Returns.Kind == rpc.ReturnSetOf {
		body, aerr := marshalCall(vals)
		if aerr != nil {
			return nil, aerr
		}
		out.body = body
		return out, nil
	}

	// A plain scalar is the single value, or JSON null when the function produced
	// no row.
	var single any
	if len(vals) > 0 {
		single = vals[0]
	}
	body, aerr := marshalCall(single)
	if aerr != nil {
		return nil, aerr
	}
	out.body = body
	return out, nil
}

// renderVoid shapes a void-returning function: PostgREST answers 200 with a null
// JSON body, never 204, so dbrest pins the same contract across backends rather
// than letting a portable scalar-with-no-rows or a native 204 special case decide
// it. The result is drained so the statement runs to completion, then discarded.
func renderVoid(res backend.Result) (*rendered, *pgerr.APIError) {
	rs := res.Rows()
	defer rs.Close()
	for rs.Next() {
		if _, err := rs.Values(); err != nil {
			return nil, pgerr.ErrInternal(err.Error())
		}
	}
	if err := rs.Err(); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}
	return &rendered{
		body:        []byte("null"),
		contentType: "application/json; charset=utf-8",
	}, nil
}

// rawJSONValue embeds a json-declared scalar verbatim. An engine expression
// (a registry SELECT json_object(...), say) carries no column type the driver
// could key the conversion on, so the declared return type decides here: a
// valid-JSON string under a json/jsonb declaration becomes a RawMessage and
// the encoder emits the document rather than a quoted string, matching how
// PostgreSQL functions returning json behave through PostgREST.
func rawJSONValue(v any, declared string) any {
	if declared != "json" && declared != "jsonb" {
		return v
	}
	if s, ok := v.(string); ok && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return v
}

// marshalCall encodes one RPC value (a scalar or an array of scalars) to JSON
// without HTML escaping and without the trailing newline the encoder appends.
func marshalCall(v any) ([]byte, *pgerr.APIError) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// renderRows shapes a backend row stream into a PostgREST-shaped JSON array,
// reproducing PostgREST's null handling (SQL NULL -> JSON null) and select-order
// keys. singular asks for a single object instead of an array and enforces the
// PGRST116 zero-or-many rule.
//
// This is the Go-shaped assembly path (Result.Rows). The engine-assembled path
// (Result.Body) is used once the embedding subsystem emits in-engine JSON; the
// observable body is identical either way.
func renderRows(res backend.Result, singular bool, rawCols map[string]bool, stripNulls bool) (*rendered, *pgerr.APIError) {
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
		// Encode each row independently so a large result streams in bounded
		// memory once the engine-assembled path replaces this shaper.
		rb, err := encodeRowObject(cols, vals, rawCols, stripNulls)
		if err != nil {
			return nil, pgerr.ErrInternal(err.Error())
		}
		rows = append(rows, json.RawMessage(rb))
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
			return nil, pgerr.ErrSingularZeroMany().
				WithDetails(fmt.Sprintf("The result contains %d rows", len(rows)))
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

// encodeRowObject serializes one row as a JSON object whose keys appear in
// projection (column) order, the way PostgREST preserves select order rather
// than the alphabetical order a Go map would impose. A rawCols column carries
// engine-assembled JSON emitted verbatim; every other value is encoded normally.
func encodeRowObject(cols []string, vals []any, rawCols map[string]bool, stripNulls bool) ([]byte, error) {
	var rb bytes.Buffer
	rb.WriteByte('{')
	first := true
	for i, c := range cols {
		var v any
		if rawCols[c] {
			v = rawJSON(vals[i])
		} else {
			v = vals[i]
		}
		// nulls=stripped drops a key whose value is SQL NULL (a nil after the raw
		// embed unwrap), so the object omits it entirely.
		if stripNulls && v == nil {
			continue
		}
		if !first {
			rb.WriteByte(',')
		}
		first = false
		key, err := jsonNoEscape(c)
		if err != nil {
			return nil, err
		}
		rb.Write(key)
		rb.WriteByte(':')
		val, err := jsonNoEscape(v)
		if err != nil {
			return nil, err
		}
		rb.Write(val)
	}
	rb.WriteByte('}')
	return rb.Bytes(), nil
}

// jsonNoEscape encodes a value to JSON the way PostgREST does: HTML characters
// stay unescaped and the encoder's trailing newline is trimmed.
func jsonNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// renderCSV writes a header row of column names followed by one RFC 4180 record
// per row. A nested value (an embedded relation or a JSON column) is serialized
// as its JSON text inside a single cell rather than expanded into more columns,
// matching PostgREST. SQL NULL renders as an empty field.
func renderCSV(res backend.Result) (*rendered, *pgerr.APIError) {
	rs := res.Rows()
	defer rs.Close()
	cols := rs.Columns()

	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	if err := cw.Write(cols); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}

	n := 0
	rec := make([]string, len(cols))
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			return nil, pgerr.ErrInternal(err.Error())
		}
		for i := range cols {
			rec[i] = csvCell(vals[i])
		}
		if err := cw.Write(rec); err != nil {
			return nil, pgerr.ErrInternal(err.Error())
		}
		n++
	}
	if err := rs.Err(); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}

	out := &rendered{body: buf.Bytes(), contentType: "text/csv; charset=utf-8", nRows: n}
	if total, ok := res.Count(); ok {
		out.total, out.hasTotl = total, true
	}
	return out, nil
}

// rawJSON wraps an engine-assembled JSON value (an embedded relation's object or
// array, already valid JSON text) so the encoder emits it verbatim instead of
// quoting it as a string. A NULL to-one embed stays nil and renders as JSON null.
func rawJSON(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return json.RawMessage(t)
	case []byte:
		return json.RawMessage(t)
	default:
		return v
	}
}

// csvCell formats one value for a CSV cell. Scalars become their text form; a
// nested map or slice becomes JSON text; NULL becomes the empty string.
func csvCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	case bool:
		// PostgreSQL's text output (what PostgREST's CSV mirrors) renders booleans
		// as t/f, not the JSON true/false.
		if t {
			return "t"
		}
		return "f"
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return fmt.Sprint(t)
	}
}

// renderScalar produces a single scalar body for application/octet-stream (raw
// bytes) and text/plain (text). The projection must resolve to exactly one
// column; the value of the first row is emitted with no JSON quoting, the way
// PostgREST serves a stored blob or a single text field.
func renderScalar(res backend.Result, asText bool) (*rendered, *pgerr.APIError) {
	rs := res.Rows()
	defer rs.Close()
	cols := rs.Columns()
	if len(cols) != 1 {
		return nil, pgerr.ErrParse("application/octet-stream and text/plain require a single-column projection")
	}

	n := 0
	var first any
	for rs.Next() {
		if n == 0 {
			vals, err := rs.Values()
			if err != nil {
				return nil, pgerr.ErrInternal(err.Error())
			}
			first = vals[0]
		}
		n++
	}
	if err := rs.Err(); err != nil {
		return nil, pgerr.ErrInternal(err.Error())
	}

	var body []byte
	switch t := first.(type) {
	case nil:
		body = nil
	case []byte:
		body = t
	case string:
		body = []byte(t)
	default:
		body = []byte(csvCell(t))
	}

	ct := "application/octet-stream"
	if asText {
		ct = "text/plain; charset=utf-8"
	}
	out := &rendered{body: body, contentType: ct, nRows: n}
	if total, ok := res.Count(); ok {
		out.total, out.hasTotl = total, true
	}
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
