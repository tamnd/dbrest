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
	"strconv"
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
	backend      backend.Backend
	model        *schema.Model
	searchPath   []string
	role         string
	verifier     *auth.Verifier
	authz        *authz.Registry
	openapiMode  string
	openapiProxy string
	corsOrigins  []string // server-cors-allowed-origins; empty means any
	maxRows      int      // db-max-rows; 0 means no cap
	planEnabled  bool     // db-plan-enabled; plans are off by default
	preRequest   string   // db-pre-request, carried to the backend per request
	appSettings  map[string]string
	logQuery     bool // log-query, carried to the backend per request
}

// NewServer builds a Server over a backend, its introspected model, and the
// schema search path (the exposed schemas, in resolution order). It has no
// default role: until SetDefaultRole or SetVerifier provides an identity
// source, every request is refused with 401 PGRST302, matching PostgREST's
// fail-closed posture when db-anon-role is unset.
func NewServer(b backend.Backend, model *schema.Model, searchPath []string) *Server {
	return &Server{backend: b, model: model, searchPath: searchPath}
}

// SetOpenAPI configures the root document. mode is the openapi-mode option:
// "disabled" turns the root off (a request there is 404); the two privilege
// modes leave it on. proxyURI, when set, is the externally visible base URL the
// document advertises (the openapi-server-proxy-uri option), overriding the
// host and scheme the request arrived on so a document served behind a reverse
// proxy points at the public address. See spec 20.
func (s *Server) SetOpenAPI(mode, proxyURI string) {
	s.openapiMode = mode
	s.openapiProxy = proxyURI
}

// SetDefaultRole sets the static role used for unauthenticated requests when no
// verifier is configured. It should be called with the db-anon-role option
// value; left unset, tokenless requests are refused with 401 PGRST302.
func (s *Server) SetDefaultRole(role string) {
	if role != "" {
		s.role = role
	}
}

// SetMaxRows applies the db-max-rows option: a hard cap on the rows any read
// or RPC response may return, enforced as an implicit LIMIT at plan time. Zero
// means no cap. Mutation representations are exempt, matching PostgREST v10+.
func (s *Server) SetMaxRows(n int) { s.maxRows = n }

// MaxRows reports the configured db-max-rows cap (0 when uncapped). The
// count=estimated logic uses it as the exactness threshold.
func (s *Server) MaxRows() int { return s.maxRows }

// capLimit lowers *limit to the db-max-rows cap, installing the cap as the
// limit when the client did not ask for one. It returns the (possibly
// replaced) pointer so callers can assign it back into the query.
func (s *Server) capLimit(limit *int) *int {
	if s.maxRows <= 0 {
		return limit
	}
	if limit == nil || *limit > s.maxRows {
		capped := s.maxRows
		return &capped
	}
	return limit
}

// SetCORSAllowedOrigins restricts cross-origin requests to the given origin
// list (the server-cors-allowed-origins option). With an empty list the server
// keeps the PostgREST default: any origin is accepted.
func (s *Server) SetCORSAllowedOrigins(origins []string) { s.corsOrigins = origins }

// SetPlanEnabled applies the db-plan-enabled option. Execution plans leak
// schema and statistics detail, so PostgREST only honors the
// application/vnd.pgrst.plan+json media type when the option is on; the
// default is off, and a plan request then fails the same way as any other
// unproducible media type.
func (s *Server) SetPlanEnabled(on bool) { s.planEnabled = on }

// SetAppSettings carries the app.settings.* options to the backend on every
// request context, to be applied as transaction settings.
func (s *Server) SetAppSettings(settings map[string]string) { s.appSettings = settings }

// SetLogQuery asks backends to echo the statements they execute, the
// log-query option.
func (s *Server) SetLogQuery(on bool) { s.logQuery = on }

// SetVerifier attaches a JWT verifier. Once set, the role and claims of each
// request come from its bearer token (spec 13), and a bad token is rejected
// before any query runs. With no verifier the server keeps the static role.
func (s *Server) SetVerifier(v *auth.Verifier) { s.verifier = v }

// SetPreRequest names the db-pre-request function carried to the backend on
// every request context. The backend invokes it after the request context is
// in place and before the main statement (spec 13); the caller is responsible
// for refusing the option at startup on a backend that cannot honor it.
func (s *Server) SetPreRequest(fn string) { s.preRequest = fn }

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
// boundary (method, path, headers, cookies, and the selected schema), and the
// configured transaction-scoped settings (db-pre-request, app.settings.*,
// log-query). The frontend builds it once after authentication; on the
// emulated backend the values a policy references are later bound as
// parameters (spec 15).
func (s *Server) buildContext(r *http.Request, id identity) *reqctx.Context {
	cookies := r.Cookies()
	jar := make(map[string]string, len(cookies))
	for _, c := range cookies {
		jar[c.Name] = c.Value
	}
	return &reqctx.Context{
		Role:        id.role,
		Anonymous:   id.anonymous,
		Claims:      id.claims,
		Method:      r.Method,
		Path:        r.URL.Path,
		Headers:     r.Header,
		Cookies:     jar,
		Schema:      requestSchema(r),
		PreRequest:  s.preRequest,
		AppSettings: s.appSettings,
		LogQuery:    s.logQuery,
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
// bearer token to a role (or anon), or returns the 401/403 the token earns. The
// no-verifier path fails closed: with no default role configured, tokenless
// requests are refused with 401 PGRST302 rather than run as anyone.
func (s *Server) authenticate(r *http.Request) (identity, *pgerr.APIError) {
	if s.verifier == nil {
		if s.role == "" {
			return identity{}, pgerr.ErrJWTRequired()
		}
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
	if s.serveCORS(w, r) {
		return
	}
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

// corsExposedHeaders is the Access-Control-Expose-Headers value PostgREST
// returns on every cross-origin request.
const corsExposedHeaders = "Content-Encoding, Content-Location, Content-Range, Content-Type, " +
	"Date, Location, Server, Transfer-Encoding, Range-Unit"

// corsAllowedMethods is the Access-Control-Allow-Methods value PostgREST
// returns on a preflight.
const corsAllowedMethods = "GET, POST, PATCH, PUT, DELETE, OPTIONS, HEAD"

// serveCORS answers CORS the way PostgREST v14 does and reports whether the
// request was fully handled (a preflight). A request without an Origin header
// is untouched. With server-cors-allowed-origins unset any origin is accepted
// with Access-Control-Allow-Origin: *; with the option set, a listed origin is
// reflected with Access-Control-Allow-Credentials: true and an unlisted one
// falls through to normal handling with no CORS headers (the browser enforces
// the denial). A preflight (OPTIONS with Access-Control-Request-Method) is
// answered directly with the allowed methods, the requested headers, and a
// one-day max age, before authentication and routing.
func (s *Server) serveCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	allowOrigin := "*"
	credentials := false
	if len(s.corsOrigins) > 0 {
		found := false
		for _, o := range s.corsOrigins {
			if o == origin {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		allowOrigin = origin
		credentials = true
	}

	h := w.Header()
	h.Set("Access-Control-Allow-Origin", allowOrigin)
	if credentials {
		h.Set("Access-Control-Allow-Credentials", "true")
	}

	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.Set("Access-Control-Allow-Methods", corsAllowedMethods)
		h.Set("Access-Control-Allow-Headers", corsAllowedHeaders(r.Header.Get("Access-Control-Request-Headers")))
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusOK)
		return true
	}

	h.Set("Access-Control-Expose-Headers", corsExposedHeaders)
	return false
}

// corsAllowedHeaders builds the preflight Access-Control-Allow-Headers value:
// Authorization, then the headers the client asked for, then the simple
// headers, deduplicated case-insensitively. The order matches PostgREST.
func corsAllowedHeaders(requested string) string {
	out := []string{"Authorization"}
	seen := map[string]bool{"authorization": true}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[strings.ToLower(name)] {
			return
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
	for _, name := range strings.Split(requested, ",") {
		add(name)
	}
	for _, name := range []string{"Accept", "Accept-Language", "Content-Language"} {
		add(name)
	}
	return strings.Join(out, ", ")
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
	// db-max-rows caps an RPC response like a read (an implicit LIMIT).
	call.Limit = s.capLimit(call.Limit)

	var planned *ir.Plan
	if s.backend.Capabilities().NativeRPC {
		// PostgreSQL (and any other NativeRPC backend) discovers and executes
		// the function from its own catalog. We skip the portable-registry lookup
		// and build a minimal plan: ReadOnly follows the HTTP method (GET/HEAD
		// means read-only; POST means the function may write). The engine enforces
		// the volatility constraint — if a GET reaches a volatile function the
		// read-only transaction fails with SQLSTATE 25006, which maps to 405.
		planned = &ir.Plan{Call: call, ReadOnly: isGet}
	} else {
		planned, apiErr = plan.Call(s.backend.Functions(), call, isGet, s.searchPath)
		if apiErr != nil {
			writeError(w, apiErr)
			return
		}
	}

	rc := s.buildContext(r, id)
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, mapExecError(s.backend, err, id.anonymous))
		return
	}

	out, apiErr := renderCall(media, res, planned.Func, fn)
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

	acceptHdrs := r.Header.Values("Accept")
	media, ok := negotiate(acceptHdrs)
	if !ok {
		writeError(w, pgerr.ErrNotAcceptable(strings.Join(acceptHdrs, ", ")))
		return
	}

	q, apiErr := ir.ParseRead(relation, r.URL.RawQuery, r.Header.Values("Prefer"))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	q.Singular = media == mediaObject

	// Range: header overrides ?limit=&offset= and marks the request as a
	// Range request so the server can return 206 Partial Content. PostgREST
	// accepts Range: 0-9 (item range) without requiring Range-Unit: items.
	// Only treat Range as item pagination when it has no unit prefix (i.e.
	// not "bytes=0-9" form), matching PostgREST's parsing behaviour.
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" && !strings.Contains(rangeHdr, "=") {
		if off, lim, ok := parseRangeHeader(rangeHdr); ok {
			q.Offset = &off
			if lim >= 0 {
				l := lim
				q.Limit = &l
				q.FromRange = true // bounded Range → eligible for 206
			}
		}
	}

	// db-max-rows is a hard cap on every read: the effective window is
	// min(requested limit, max-rows), applied before planning so Content-Range
	// and the 200/206 decision see the limit that actually ran. Mutation
	// representations are exempt (PostgREST v10+), so this stays off the
	// write path.
	q.Limit = s.capLimit(q.Limit)

	planned, apiErr := plan.Read(s.model, q, s.searchPath)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	rc := s.buildContext(r, id)

	if apiErr := s.authorize(rc, planned); apiErr != nil {
		writeError(w, apiErr)
		return
	}

	// vnd.pgrst.plan+json: return EXPLAIN JSON when the backend supports it.
	// The db-plan-enabled gate comes first: with the option off the media
	// type is not producible at all, whatever the backend can do.
	if media == mediaPlan {
		if !s.planEnabled {
			writeError(w, pgerr.ErrNotAcceptable(mediaPlan))
			return
		}
		exp, supported := s.backend.(backend.Explainer)
		if !supported {
			writeError(w, pgerr.ErrNotAcceptable(mediaPlan))
			return
		}
		planJSON, err := exp.ExplainRead(r.Context(), planned, rc, planAnalyze(acceptHdrs))
		if err != nil {
			writeError(w, mapExecError(s.backend, err, id.anonymous))
			return
		}
		w.Header().Set("Content-Type", mediaPlan)
		w.WriteHeader(http.StatusOK)
		w.Write(planJSON)
		return
	}

	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, mapExecError(s.backend, err, id.anonymous))
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

	rc := s.buildContext(r, id)
	if apiErr := s.authorize(rc, planned); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, mapExecError(s.backend, err, id.anonymous))
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
	// PostgREST v14 returns a Location header only for return=headers-only inserts/upserts.
	// For return=representation or minimal, Location is omitted.
	if (q.Kind == ir.Insert || q.Kind == ir.Upsert) && q.Write != nil && q.Write.Return == ir.ReturnHeadersOnly {
		if loc := locationHeader(rel, q.Relation.Name, res); loc != "" {
			w.Header().Set("Location", loc)
		}
	}

	representation := q.Write.Return == ir.ReturnRepresentation
	if !representation {
		// When count=exact was requested, include Content-Range: */<n> so the
		// client knows how many rows were affected, matching PostgREST's wire.
		if q.Count == ir.CountExact {
			if n, ok := res.Affected(); ok {
				w.Header().Set("Content-Range", fmt.Sprintf("*/%d", n))
			}
		}
		w.WriteHeader(applyControls(w, ctrl, writeStatus(r.Method, q.Kind, false, ctrl)))
		return
	}

	out, apiErr := renderFor(media, res, embedKeys(q))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	w.Header().Set("Content-Type", out.contentType)
	if !q.Singular {
		// For writes with count=exact, include the total in Content-Range.
		if q.Count == ir.CountExact {
			if n, ok := res.Affected(); ok {
				w.Header().Set("Content-Range", contentRange(0, out.nRows, n, true))
			} else {
				w.Header().Set("Content-Range", contentRange(0, out.nRows, 0, false))
			}
		} else {
			w.Header().Set("Content-Range", contentRange(0, out.nRows, 0, false))
		}
	}
	w.WriteHeader(applyControls(w, ctrl, writeStatus(r.Method, q.Kind, true, ctrl)))
	if r.Method != http.MethodHead {
		w.Write(out.body)
	}
}

// writeStatus is the status for a successful write.
//   - POST insert: 201 Created.
//   - POST upsert where ALL rows were new inserts: 201 Created.
//   - POST upsert where at least one row was an ON CONFLICT update: 200 OK.
//   - PUT upsert where the row is known to be a new insert: 201 Created.
//   - PUT upsert where the row is known to be an update, or unknown: 200 OK.
//   - PATCH/DELETE with representation: 200 OK.
//   - PATCH/DELETE without representation: 204 No Content.
func writeStatus(method string, kind ir.QueryKind, representation bool, ctrl *reqctx.ResponseControls) int {
	if method == http.MethodPost {
		// 200 when the upsert hit at least one existing row (ON CONFLICT UPDATE fired).
		// 201 otherwise (new row was inserted or unknown).
		if kind == ir.Upsert && ctrl != nil && ctrl.UpsertStatusKnown && !ctrl.UpsertInsert {
			return http.StatusOK
		}
		return http.StatusCreated
	}
	if method == http.MethodPut && kind == ir.Upsert {
		// PUT is semantically "create or replace"; default to 200.
		// Only return 201 when the backend positively confirms a new insert.
		if ctrl != nil && ctrl.UpsertStatusKnown && ctrl.UpsertInsert {
			return http.StatusCreated
		}
		return http.StatusOK
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
	if applied := q.Prefer.AppliedHeader(); applied != "" {
		w.Header().Set("Preference-Applied", applied)
	}
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

// readStatus applies PostgREST's 200/206 rule: 206 only when a count is known
// and the page returned is genuinely partial (nRows < total). PostgREST v14
// returns 200 for count=planned/estimated even though the total is approximate;
// the estimate is informational, not a range boundary.
func readStatus(q *ir.Query, out *rendered, _ int) int {
	if !out.hasTotl {
		return http.StatusOK
	}
	if q.Count == ir.CountExact && int64(out.nRows) < out.total {
		return http.StatusPartialContent
	}
	return http.StatusOK
}

// parseRangeHeader parses an HTTP Range header value of the form "start-end"
// (as used with Range-Unit: items). Returns (offset, limit, true) where limit
// is -1 for an open-ended range ("0-"). Returns (0, 0, false) on parse error.
func parseRangeHeader(s string) (offset, limit int, ok bool) {
	dash := strings.LastIndex(s, "-")
	if dash < 0 {
		return 0, 0, false
	}
	startStr, endStr := s[:dash], s[dash+1:]
	start, err := strconv.Atoi(startStr)
	if err != nil || start < 0 {
		return 0, 0, false
	}
	if endStr == "" {
		return start, -1, true // open-ended: "0-"
	}
	end, err := strconv.Atoi(endStr)
	if err != nil || end < start {
		return 0, 0, false
	}
	return start, end - start + 1, true
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

// mapExecError wraps asAPIError with the PostgREST 401/403 rule: a 42501
// (insufficient_privilege) error to an anonymous request is 401 (authentication
// required), not 403 (forbidden). An authenticated request that is denied
// remains 403 so the caller knows to authenticate, not just retry.
// The original PostgreSQL message is preserved to match PostgREST wire behavior.
func mapExecError(b backend.Backend, err error, anonymous bool) *pgerr.APIError {
	e := asAPIError(b, err)
	if anonymous && e.Code == pgerr.CodeInsufficientPrivilege {
		lifted := *e
		lifted.HTTPStatus = http.StatusUnauthorized
		// PostgREST sends the bare Bearer challenge on every 401, including a
		// privilege denial lifted from 403 for an unauthenticated request.
		lifted.WWWAuthenticate = "Bearer"
		return &lifted
	}
	return e
}

func writeError(w http.ResponseWriter, e *pgerr.APIError) {
	e.Write(w)
}
