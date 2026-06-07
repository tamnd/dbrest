// Package httpapi is the HTTP frontend: it routes a request, drives the
// parse -> plan -> execute -> render pipeline, and writes a PostgREST-shaped
// response. It is backend-agnostic; it talks only to the backend SPI and the
// schema model. See spec 10-reads.
package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/plan"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// singularMediaType is the Accept value that asks for a single object.
const singularMediaType = "application/vnd.pgrst.object+json"

// maxBodyBytes caps a request body, so a runaway payload cannot exhaust memory.
const maxBodyBytes = 16 << 20 // 16 MiB

// Server holds the resolved schema model and the backend, and serves the read
// API. Writes, RPC, and auth are added with their subsystems; the router rejects
// what it does not yet handle with an honest error rather than a wrong answer.
type Server struct {
	backend    backend.Backend
	model      *schema.Model
	searchPath []string
	role       string
}

// NewServer builds a Server over a backend, its introspected model, and the
// schema search path (the exposed schemas, in resolution order).
func NewServer(b backend.Backend, model *schema.Model, searchPath []string) *Server {
	return &Server{backend: b, model: model, searchPath: searchPath, role: "anon"}
}

// ServeHTTP routes the request by method onto a /<table> resource. GET/HEAD
// read; POST inserts (or upserts when the client asks to resolve duplicates);
// PATCH updates; PUT upserts; DELETE deletes. RPC and OpenAPI arrive with their
// subsystems; an unhandled method gets an honest error.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleRead(w, r)
	case http.MethodPost:
		s.handleWrite(w, r, ir.Insert)
	case http.MethodPatch:
		s.handleWrite(w, r, ir.Update)
	case http.MethodPut:
		s.handleWrite(w, r, ir.Upsert)
	case http.MethodDelete:
		s.handleWrite(w, r, ir.Delete)
	default:
		writeError(w, pgerr.ErrUnsupported(r.Method+" requests", "dbrest"))
	}
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	relation := strings.Trim(r.URL.Path, "/")
	if relation == "" || strings.Contains(relation, "/") {
		writeError(w, pgerr.ErrUnknownTable(relation))
		return
	}

	q, apiErr := ir.ParseRead(relation, r.URL.RawQuery, r.Header.Values("Prefer"))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	q.Singular = wantsSingular(r)

	planned, apiErr := plan.Read(s.model, q, s.searchPath)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	rc := &reqctx.Context{
		Role:    s.role,
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: r.Header,
	}

	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, asAPIError(s.backend, err))
		return
	}

	out, apiErr := renderRows(res, q.Singular)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	s.writeRead(w, r, q, out)
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, kind ir.QueryKind) {
	relation := strings.Trim(r.URL.Path, "/")
	if relation == "" || strings.Contains(relation, "/") {
		writeError(w, pgerr.ErrUnknownTable(relation))
		return
	}

	var body []byte
	if kind != ir.Delete {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, pgerr.ErrParse("could not read request body"))
			return
		}
		body = b
	}

	q, apiErr := ir.ParseWrite(kind, relation, r.URL.RawQuery, r.Header.Values("Prefer"), body)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	q.Singular = wantsSingular(r)

	planned, apiErr := plan.Write(s.model, q, s.searchPath)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	rc := &reqctx.Context{Role: s.role, Method: r.Method, Path: r.URL.Path, Headers: r.Header}
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, asAPIError(s.backend, err))
		return
	}

	s.writeWrite(w, r, q, planned.Rel, res)
}

// writeWrite sets headers, status, and body for a successful write. A
// representation returns the affected rows (and Content-Range for a collection);
// otherwise the body is empty. An insert or upsert of a single row carries a
// Location header pointing at the new resource by primary key.
func (s *Server) writeWrite(w http.ResponseWriter, r *http.Request, q *ir.Query, rel *schema.Relation, res backend.Result) {
	for k, v := range res.ResponseControls().Headers {
		w.Header().Set(k, v)
	}
	if applied := q.Prefer.AppliedHeader(); applied != "" {
		w.Header().Set("Preference-Applied", applied)
	}
	if q.Kind == ir.Insert || q.Kind == ir.Upsert {
		if loc := locationHeader(rel, q.Relation.Name, res); loc != "" {
			w.Header().Set("Location", loc)
		}
	}

	representation := q.Write.Return == ir.ReturnRepresentation
	if !representation {
		w.WriteHeader(writeStatus(r.Method, false))
		return
	}

	out, apiErr := renderRows(res, q.Singular)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if q.Singular {
		w.Header().Set("Content-Type", singularMediaType+"; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Range", contentRange(0, out.nRows, 0, false))
	}
	w.WriteHeader(writeStatus(r.Method, true))
	if r.Method != http.MethodHead {
		w.Write(out.body)
	}
}

// writeStatus is the status for a successful write: POST is always 201 Created;
// the other methods are 200 when they return a representation and 204 No Content
// when they do not.
func writeStatus(method string, representation bool) int {
	if method == http.MethodPost {
		return http.StatusCreated
	}
	if representation {
		return http.StatusOK
	}
	return http.StatusNoContent
}

// locationHeader builds the Location for a single inserted or upserted row from
// its primary key, e.g. /films?id=eq.5. It returns "" when the relation has no
// primary key, the key columns are not in the result, or more than one row was
// written.
func locationHeader(rel *schema.Relation, relation string, res backend.Result) string {
	if len(rel.PrimaryKey) == 0 {
		return ""
	}
	rs := res.Rows()
	defer rs.Close()
	idx := make(map[string]int, len(rs.Columns()))
	for i, c := range rs.Columns() {
		idx[c] = i
	}
	if !rs.Next() {
		return ""
	}
	vals, err := rs.Values()
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(rel.PrimaryKey))
	for _, pk := range rel.PrimaryKey {
		i, ok := idx[pk]
		if !ok {
			return ""
		}
		parts = append(parts, url.QueryEscape(pk)+"=eq."+url.QueryEscape(fmt.Sprint(vals[i])))
	}
	if rs.Next() {
		// More than one row written: no single resource to point at.
		return ""
	}
	return "/" + relation + "?" + strings.Join(parts, "&")
}

// writeRead sets the headers and status for a successful read and writes the
// body (omitted for HEAD).
func (s *Server) writeRead(w http.ResponseWriter, r *http.Request, q *ir.Query, out *rendered) {
	if q.Singular {
		w.Header().Set("Content-Type", singularMediaType+"; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}

	offset := 0
	if q.Offset != nil {
		offset = *q.Offset
	}
	w.Header().Set("Content-Range", contentRange(offset, out.nRows, out.total, out.hasTotl))

	// An out-of-range offset is 416: the window starts past the end of the
	// result. This is only knowable with a count, so it applies when one was
	// requested (otherwise the empty window is a plain 200 with an empty array).
	if offset > 0 && out.hasTotl && int64(offset) >= out.total {
		rng := pgerr.ErrRangeNotSatisfiable()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(rng.HTTPStatus)
		if r.Method != http.MethodHead {
			w.Write(rng.JSON())
		}
		return
	}

	w.WriteHeader(readStatus(q, out, offset))
	if r.Method != http.MethodHead {
		w.Write(out.body)
	}
}

// readStatus applies PostgREST's 200/206 rule: 206 when a bounded window was
// requested and does not cover the whole result, 200 otherwise. When a count is
// present, a window that returned every matching row is 200; without a count,
// any window is treated as partial.
func readStatus(q *ir.Query, out *rendered, _ int) int {
	hasWindow := q.Limit != nil || q.Offset != nil
	if !hasWindow {
		return http.StatusOK
	}
	if out.hasTotl && int64(out.nRows) >= out.total {
		return http.StatusOK
	}
	return http.StatusPartialContent
}

func wantsSingular(r *http.Request) bool {
	for _, a := range r.Header.Values("Accept") {
		if strings.Contains(a, singularMediaType) {
			return true
		}
	}
	return false
}

// asAPIError normalizes a backend execution error to the API envelope, asking
// the backend to map an engine-native error and falling back to whatever the
// error already is or an internal error.
func asAPIError(b backend.Backend, err error) *pgerr.APIError {
	if e := pgerr.As(err); e != nil {
		return e
	}
	if mapped := b.MapError(err); mapped != nil {
		return mapped
	}
	return pgerr.ErrInternal(err.Error())
}

func writeError(w http.ResponseWriter, e *pgerr.APIError) {
	e.Write(w)
}
