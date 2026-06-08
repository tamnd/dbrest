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

	"github.com/tamnd/dbrest/auth"
	"github.com/tamnd/dbrest/authz"
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

// Server holds the resolved schema model and the backend, and serves the API. A
// verifier, when set, resolves the request role from the JWT; with none, every
// request runs as the static default role.
type Server struct {
	backend    backend.Backend
	model      *schema.Model
	searchPath []string
	role       string
	verifier   *auth.Verifier
	authz      *authz.Registry
}

// NewServer builds a Server over a backend, its introspected model, and the
// schema search path (the exposed schemas, in resolution order). It runs every
// request as the anon role until a verifier is attached with SetVerifier.
func NewServer(b backend.Backend, model *schema.Model, searchPath []string) *Server {
	return &Server{backend: b, model: model, searchPath: searchPath, role: "anon"}
}

// SetVerifier attaches a JWT verifier. Once set, the role and claims of each
// request come from its bearer token (spec 13), and a bad token is rejected
// before any query runs. With no verifier the server keeps the static role.
func (s *Server) SetVerifier(v *auth.Verifier) { s.verifier = v }

// SetAuthz attaches an authorization registry. Once set, every read and write is
// gated by the registry's table and column privileges and has any Row Level
// Security policy injected before execution (spec 14). On a backend whose engine
// enforces its own row security this stays nil and the engine decides; on the
// emulated backends the registry is the security boundary.
func (s *Server) SetAuthz(r *authz.Registry) { s.authz = r }

// authorize runs the authorization gate on a planned query when a registry is
// attached. It mutates the plan in place (narrowing a projection, injecting a
// policy predicate) and returns the denial error otherwise. The anonymous flag
// selects 401 over 403 for an unauthenticated request.
func (s *Server) authorize(rc *reqctx.Context, planned *ir.Plan) *pgerr.APIError {
	if s.authz == nil {
		return nil
	}
	return s.authz.Authorize(rc, planned)
}

// identity is the resolved per-request principal: the role the request runs as
// and the verified JWT claims, if any. It is built fresh per request and never
// stored on the Server, which is shared across goroutines.
type identity struct {
	role      string
	claims    map[string]any
	anonymous bool
}

// buildContext assembles the per-request context the backend receives: the
// resolved identity plus the request metadata that crosses the HTTP/query
// boundary (method, path, headers, cookies, and the selected schema). The
// frontend builds it once after authentication; on the emulated backend the
// values a policy references are later bound as parameters (spec 15).
func buildContext(r *http.Request, id identity) *reqctx.Context {
	cookies := r.Cookies()
	jar := make(map[string]string, len(cookies))
	for _, c := range cookies {
		jar[c.Name] = c.Value
	}
	return &reqctx.Context{
		Role:      id.role,
		Anonymous: id.anonymous,
		Claims:    id.claims,
		Method:    r.Method,
		Path:      r.URL.Path,
		Headers:   r.Header,
		Cookies:   jar,
		Schema:    requestSchema(r),
	}
}

// requestSchema reads the schema the client selected with the Accept-Profile
// header (reads) or the Content-Profile header (writes). It carries the choice
// onto the context; cross-schema identifier routing is the introspection
// subsystem's job (spec 08), so an unset header is the default schema.
func requestSchema(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return r.Header.Get("Accept-Profile")
	}
	return r.Header.Get("Content-Profile")
}

// applyControls applies a backend's response controls and returns the status to
// write: each control header is set on the response, and a non-zero control
// status overrides the computed default. It runs on every path (read, write,
// RPC) so a function or policy that shapes the response does so identically
// regardless of backend (spec 15).
func applyControls(w http.ResponseWriter, rc *reqctx.ResponseControls, def int) int {
	for k, v := range rc.Headers {
		w.Header().Set(k, v)
	}
	if rc.Status != 0 {
		return rc.Status
	}
	return def
}

// authenticate resolves the request identity from the Authorization header. With
// no verifier it is the static default role; otherwise the verifier maps the
// bearer token to a role (or anon), or returns the 401/403 the token earns.
func (s *Server) authenticate(r *http.Request) (identity, *pgerr.APIError) {
	if s.verifier == nil {
		return identity{role: s.role, anonymous: true}, nil
	}
	res, apiErr := s.verifier.Authenticate(r.Header.Get("Authorization"))
	if apiErr != nil {
		return identity{}, apiErr
	}
	return identity{role: res.Role, claims: res.Claims, anonymous: res.Anonymous}, nil
}

// ServeHTTP routes the request by method onto a /<table> resource. GET/HEAD
// read; POST inserts (or upserts when the client asks to resolve duplicates);
// PATCH updates; PUT upserts; DELETE deletes. RPC and OpenAPI arrive with their
// subsystems; an unhandled method gets an honest error.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, apiErr := s.authenticate(r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if fn, ok := rpcName(r.URL.Path); ok {
		s.handleRPC(w, r, fn, id)
		return
	}
	if r.URL.Path == "/" {
		s.handleRoot(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleRead(w, r, id)
	case http.MethodPost:
		s.handleWrite(w, r, ir.Insert, id)
	case http.MethodPatch:
		s.handleWrite(w, r, ir.Update, id)
	case http.MethodPut:
		s.handleWrite(w, r, ir.Upsert, id)
	case http.MethodDelete:
		s.handleWrite(w, r, ir.Delete, id)
	default:
		writeError(w, pgerr.ErrUnsupported(r.Method+" requests", "dbrest"))
	}
}

// rpcName extracts the function name from an /rpc/<fn> path, reporting false for
// any other path. A name with a further slash (a sub-path under the function) is
// not a valid call target and is rejected by the caller as an unknown function.
func rpcName(path string) (string, bool) {
	const prefix = "/rpc/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}

// handleRPC serves a /rpc/<fn> call. GET and HEAD read (arguments from the query
// string); POST reads or writes (arguments from the JSON body). A read method may
// only reach a read-only function; the plan raises 405 otherwise. Any other
// method is not allowed on a function. See spec 12-rpc.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request, fn string, id identity) {
	if fn == "" || strings.Contains(fn, "/") {
		writeError(w, pgerr.ErrNoFunction(fn))
		return
	}

	isGet := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !isGet && r.Method != http.MethodPost {
		writeError(w, pgerr.ErrMethodNotAllowed(
			"Method "+r.Method+" not allowed on a function; use GET or POST"))
		return
	}

	media, ok := negotiate(r.Header.Values("Accept"))
	if !ok {
		writeError(w, pgerr.ErrNotAcceptable(strings.Join(r.Header.Values("Accept"), ", ")))
		return
	}

	var body []byte
	if r.Method == http.MethodPost {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, pgerr.ErrParse("could not read request body"))
			return
		}
		body = b
	}

	call, apiErr := ir.ParseCall(fn, r.URL.RawQuery, r.Header.Values("Prefer"), isGet, r.Header.Get("Content-Type"), body)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	call.Singular = media == mediaObject

	planned, apiErr := plan.Call(s.backend.Functions(), call, isGet, s.searchPath)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	rc := buildContext(r, id)
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, asAPIError(s.backend, err))
		return
	}

	out, apiErr := renderCall(media, res, planned.Func)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	s.writeCall(w, r, call, out, res.ResponseControls())
}

// writeCall writes a successful RPC response. The status is 200, or 206 when a
// bounded window over a table return did not cover the full count. A requested
// count sets Content-Range, matching a read.
func (s *Server) writeCall(w http.ResponseWriter, r *http.Request, call *ir.Call, out *rendered, ctrl *reqctx.ResponseControls) {
	if applied := call.Prefer.AppliedHeader(); applied != "" {
		w.Header().Set("Preference-Applied", applied)
	}
	w.Header().Set("Content-Type", out.contentType)

	offset := 0
	if call.Offset != nil {
		offset = *call.Offset
	}
	if out.hasTotl {
		w.Header().Set("Content-Range", contentRange(offset, out.nRows, out.total, true))
	}

	status := http.StatusOK
	hasWindow := call.Limit != nil || call.Offset != nil
	if hasWindow && out.hasTotl && int64(out.nRows) < out.total {
		status = http.StatusPartialContent
	}
	w.WriteHeader(applyControls(w, ctrl, status))
	if r.Method != http.MethodHead {
		w.Write(out.body)
	}
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request, id identity) {
	relation := strings.Trim(r.URL.Path, "/")
	if relation == "" || strings.Contains(relation, "/") {
		writeError(w, pgerr.ErrUnknownTable(relation))
		return
	}

	media, ok := negotiate(r.Header.Values("Accept"))
	if !ok {
		writeError(w, pgerr.ErrNotAcceptable(strings.Join(r.Header.Values("Accept"), ", ")))
		return
	}

	q, apiErr := ir.ParseRead(relation, r.URL.RawQuery, r.Header.Values("Prefer"))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	q.Singular = media == mediaObject

	planned, apiErr := plan.Read(s.model, q, s.searchPath)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	rc := buildContext(r, id)

	if apiErr := s.authorize(rc, planned); apiErr != nil {
		writeError(w, apiErr)
		return
	}

	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, asAPIError(s.backend, err))
		return
	}

	out, apiErr := renderFor(media, res, embedKeys(q))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	s.writeRead(w, r, q, out, res.ResponseControls())
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, kind ir.QueryKind, id identity) {
	relation := strings.Trim(r.URL.Path, "/")
	if relation == "" || strings.Contains(relation, "/") {
		writeError(w, pgerr.ErrUnknownTable(relation))
		return
	}

	media, ok := negotiate(r.Header.Values("Accept"))
	if !ok {
		writeError(w, pgerr.ErrNotAcceptable(strings.Join(r.Header.Values("Accept"), ", ")))
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

	q, apiErr := ir.ParseWrite(kind, relation, r.URL.RawQuery, r.Header.Values("Prefer"), r.Header.Get("Content-Type"), body)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	q.Singular = media == mediaObject

	planned, apiErr := plan.Write(s.model, q, s.searchPath)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	rc := buildContext(r, id)
	if apiErr := s.authorize(rc, planned); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, asAPIError(s.backend, err))
		return
	}

	s.writeWrite(w, r, q, media, planned.Rel, res)
}

// writeWrite sets headers, status, and body for a successful write. A
// representation returns the affected rows (and Content-Range for a collection);
// otherwise the body is empty. An insert or upsert of a single row carries a
// Location header pointing at the new resource by primary key.
func (s *Server) writeWrite(w http.ResponseWriter, r *http.Request, q *ir.Query, media string, rel *schema.Relation, res backend.Result) {
	ctrl := res.ResponseControls()
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
		w.WriteHeader(applyControls(w, ctrl, writeStatus(r.Method, false)))
		return
	}

	out, apiErr := renderFor(media, res, embedKeys(q))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	w.Header().Set("Content-Type", out.contentType)
	if !q.Singular {
		w.Header().Set("Content-Range", contentRange(0, out.nRows, 0, false))
	}
	w.WriteHeader(applyControls(w, ctrl, writeStatus(r.Method, true)))
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

// embedKeys is the set of top-level output keys carrying an engine-assembled
// embedded resource. The renderer emits those columns as raw JSON rather than
// quoting their text. Nested embeds are already inside their parent's JSON blob,
// so only the top level matters here.
func embedKeys(q *ir.Query) map[string]bool {
	if len(q.Embeds) == 0 {
		return nil
	}
	keys := make(map[string]bool, len(q.Embeds))
	for i := range q.Embeds {
		keys[q.Embeds[i].OutKey] = true
	}
	return keys
}

// writeRead sets the headers and status for a successful read and writes the
// body (omitted for HEAD). A function or policy can shape the response through
// the controls: a control header is added and a non-zero control status wins
// over the computed 200/206 default.
func (s *Server) writeRead(w http.ResponseWriter, r *http.Request, q *ir.Query, out *rendered, ctrl *reqctx.ResponseControls) {
	w.Header().Set("Content-Type", out.contentType)

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

	w.WriteHeader(applyControls(w, ctrl, readStatus(q, out, offset)))
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
