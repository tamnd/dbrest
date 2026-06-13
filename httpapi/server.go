// Package httpapi is the HTTP frontend: it routes a request, drives the
// parse -> plan -> execute -> render pipeline, and writes a PostgREST-shaped
// response. It is backend-agnostic; it talks only to the backend SPI and the
// schema model. See spec 10-reads.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/dbrest/auth"
	"github.com/tamnd/dbrest/authz"
	"github.com/tamnd/dbrest/backend"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/plan"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/rpc"
	"github.com/tamnd/dbrest/schema"
)

// singularMediaType is the Accept value that asks for a single object.
const singularMediaType = "application/vnd.pgrst.object+json"


// Server holds the resolved schema model and the backend, and serves the API. A
// verifier, when set, resolves the request role from the JWT; with none, every
// request runs as the static default role.
type Server struct {
	backend         backend.Backend
	cache           *schema.Cache
	searchPath      []string
	role            string
	verifier        *auth.Verifier
	authz           *authz.Registry
	openapiMode     string
	openapiProxy    string
	openapiSecurity bool
	rootSpec        string
	corsOrigins     []string // server-cors-allowed-origins; empty means any
	maxRows         int      // db-max-rows; 0 means no cap
	maxBody         int64    // max-request-body bytes; 0 means unlimited
	planEnabled     bool     // db-plan-enabled; plans are off by default
	aggregatesOn    bool     // db-aggregates-enabled; aggregates are off by default
	preRequest      string   // db-pre-request, carried to the backend per request
	appSettings     map[string]string
	logQuery        bool // log-query, carried to the backend per request
	timingEnabled   bool // server-timing-enabled; the Server-Timing header is off by default
	txEnd           ir.TxEnd // db-tx-end; governs whether Prefer: tx= is honored
}

// NewServer builds a Server over a backend, its introspected model, and the
// schema search path (the exposed schemas, in resolution order). It has no
// default role: until SetDefaultRole or SetVerifier provides an identity
// source, every request is refused with 401 PGRST302, matching PostgREST's
// fail-closed posture when db-anon-role is unset.
func NewServer(b backend.Backend, model *schema.Model, searchPath []string) *Server {
	return &Server{backend: b, cache: schema.NewCache(model), searchPath: searchPath}
}

// Model returns the current schema model snapshot. A handler loads it once at
// entry so one request never straddles a reload.
func (s *Server) Model() *schema.Model { return s.cache.Load() }

// Reload re-runs introspection and publishes the fresh model, the schema
// cache reload PostgREST performs on SIGUSR1 and on NOTIFY over the
// db-channel. In-flight requests keep the snapshot they started with; a
// failed introspection leaves the old model published, so a transient
// database error never takes the running cache down. The OpenAPI document is
// generated per request from the published model and needs no separate
// regeneration.
func (s *Server) Reload(ctx context.Context) error {
	model, err := s.backend.Introspect(ctx)
	if err != nil {
		return err
	}
	s.cache.Store(model)
	return nil
}

// SetOpenAPI configures the root document. mode is the openapi-mode option:
// "disabled" turns the root off (a request there is 404); the two privilege
// modes leave it on. proxyURI, when set, is the externally visible base URL the
// document advertises (the openapi-server-proxy-uri option), overriding the
// host and scheme the request arrived on so a document served behind a reverse
// proxy points at the public address. securityActive is the
// openapi-security-active option: it attaches the JWT security requirement to
// every operation rather than just describing the scheme. See spec 20.
func (s *Server) SetOpenAPI(mode, proxyURI string, securityActive bool) {
	s.openapiMode = mode
	s.openapiProxy = proxyURI
	s.openapiSecurity = securityActive
}

// SetRootSpec names the function whose JSON result replaces the generated
// OpenAPI document, the db-root-spec option. Empty keeps the generated
// document. The function is called like GET /rpc/<fn> with no arguments.
func (s *Server) SetRootSpec(fn string) { s.rootSpec = fn }

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

// SetMaxRequestBody applies the max-request-body option: a byte cap on a
// request body. Zero, the default, leaves bodies unlimited as PostgREST does;
// a positive value is a runaway-payload guard that an operator opts into.
func (s *Server) SetMaxRequestBody(n int) { s.maxBody = int64(n) }

// readBody reads a request body, honoring the optional max-request-body cap. A
// body over the cap is a 413 with the byte bound, not a parse error; a read
// error under the cap stays the generic could-not-read parse error.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, *pgerr.APIError) {
	reader := r.Body
	if s.maxBody > 0 {
		reader = http.MaxBytesReader(w, r.Body, s.maxBody)
	}
	b, err := io.ReadAll(reader)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, pgerr.ErrBodyTooLarge(s.maxBody)
		}
		return nil, pgerr.ErrParse("could not read request body")
	}
	return b, nil
}

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

// SetAggregatesEnabled applies db-aggregates-enabled: when on, requests may use
// aggregate functions (count(), col.sum(), ...). It is off by default, matching
// PostgREST, so an aggregate request answers PGRST123 until an operator opts in.
func (s *Server) SetAggregatesEnabled(on bool) { s.aggregatesOn = on }

// SetAppSettings carries the app.settings.* options to the backend on every
// request context, to be applied as transaction settings.
func (s *Server) SetAppSettings(settings map[string]string) { s.appSettings = settings }

// SetLogQuery asks backends to echo the statements they execute, the
// log-query option.
func (s *Server) SetLogQuery(on bool) { s.logQuery = on }

// SetServerTimingEnabled applies the server-timing-enabled option. When on,
// every response carries a Server-Timing header with the jwt/parse/plan/
// transaction/response phase durations; the default is off, matching
// PostgREST, so the wire is unchanged until an operator opts in.
func (s *Server) SetServerTimingEnabled(on bool) { s.timingEnabled = on }

// SetTxEnd applies the db-tx-end option, the policy that decides whether a
// request's Prefer: tx= may override the transaction outcome. The default
// commit ignores the preference, matching PostgREST.
func (s *Server) SetTxEnd(v string) { s.txEnd = ir.ParseTxEnd(v) }

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
// boundary (method, path, headers, cookies, and the active schema), and the
// configured transaction-scoped settings (db-pre-request, app.settings.*,
// log-query). The frontend builds it once after authentication; on the
// emulated backend the values a policy references are later bound as
// parameters (spec 15).
func (s *Server) buildContext(r *http.Request, id identity, activeSchema string) *reqctx.Context {
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
		Schema:      activeSchema,
		PreRequest:  s.preRequest,
		AppSettings: s.appSettings,
		LogQuery:    s.logQuery,
	}
}

// resolveSchema negotiates the active schema for the request, the PostgREST
// profile rules: POST/PATCH/PUT/DELETE read Content-Profile, every other
// method reads Accept-Profile; no header selects the first exposed schema. A
// profile outside db-schemas is 406 PGRST106. The bool reports whether the
// schema was negotiated, which is when the client named one, or implicitly on
// a multi-schema deployment; a negotiated response echoes the active schema in
// a Content-Profile response header.
func (s *Server) resolveSchema(r *http.Request) (string, bool, *pgerr.APIError) {
	var profile string
	switch r.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		profile = r.Header.Get("Content-Profile")
	default:
		profile = r.Header.Get("Accept-Profile")
	}
	if profile == "" {
		var def string
		if len(s.searchPath) > 0 {
			def = s.searchPath[0]
		}
		return def, len(s.searchPath) > 1, nil
	}
	for _, sch := range s.searchPath {
		if sch == profile {
			return profile, true, nil
		}
	}
	return "", false, errUnacceptableSchema(profile, s.searchPath)
}

// errUnacceptableSchema is PostgREST's PGRST106: a profile header naming a
// schema that is not exposed by db-schemas, a 406 whose hint lists the schemas
// that are.
func errUnacceptableSchema(profile string, schemas []string) *pgerr.APIError {
	e := pgerr.New(http.StatusNotAcceptable, "PGRST106", "Invalid schema: "+profile)
	return e.WithHint("Only the following schemas are exposed: " + strings.Join(schemas, ", "))
}

// applyTxPolicy resolves a request's Prefer: tx= against the db-tx-end server
// policy and returns the PGRST122 a handling=strict request earns when tx= is
// disallowed. It runs after parsing and before execution on every method.
func (s *Server) applyTxPolicy(p *ir.PreferSet) *pgerr.APIError {
	p.ResolveTx(s.txEnd)
	return p.StrictError()
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
	if r.Method == http.MethodOptions {
		// OPTIONS describes the resource with an Allow header and runs no
		// transaction, so it answers before authentication and schema negotiation,
		// the way PostgREST does. A CORS preflight was already handled by serveCORS.
		s.handleOptions(w, r)
		return
	}
	// server-timing-enabled wraps the response so every exit path emits the
	// Server-Timing header, and carries a phaseTimer to the handlers through the
	// request context. The jwt phase is the only one measured here; the rest are
	// recorded inside the handlers.
	var timer *phaseTimer
	if s.timingEnabled {
		timer = &phaseTimer{}
		w = &timingWriter{ResponseWriter: w, timer: timer}
		r = r.WithContext(withTimer(r.Context(), timer))
	}
	jwtStart := time.Now()
	id, apiErr := s.authenticate(r)
	timer.mark("jwt", jwtStart)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	activeSchema, negotiated, apiErr := s.resolveSchema(r)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if negotiated {
		// PostgREST echoes the negotiated schema on successful responses so the
		// client knows which schema served it; writeError strips it on failure.
		w.Header().Set("Content-Profile", activeSchema)
	}
	if fn, ok := rpcName(r.URL.Path); ok {
		s.handleRPC(w, r, fn, id, activeSchema)
		return
	}
	if r.URL.Path == "/" {
		s.handleRoot(w, r, id, activeSchema)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleRead(w, r, id, activeSchema)
	case http.MethodPost:
		s.handleWrite(w, r, ir.Insert, id, activeSchema)
	case http.MethodPatch:
		s.handleWrite(w, r, ir.Update, id, activeSchema)
	case http.MethodPut:
		s.handleWrite(w, r, ir.Upsert, id, activeSchema)
	case http.MethodDelete:
		s.handleWrite(w, r, ir.Delete, id, activeSchema)
	default:
		// A verb the server implements nowhere (TRACE, CONNECT, a custom method)
		// is PostgREST's 405 PGRST117, not the capability gate's PGRST127.
		writeError(w, pgerr.ErrUnsupportedMethod(r.Method))
	}
}

// tableAllow is the Allow value an OPTIONS on a table or view answers with: the
// full verb set a relation endpoint accepts, in PostgREST's order.
const tableAllow = "OPTIONS,GET,HEAD,POST,PUT,PATCH,DELETE"

// handleOptions answers an OPTIONS request with an Allow header naming the
// methods the resource accepts and a 200 with no body, the way PostgREST does.
// The root answers its own verb set, a function answers by volatility (a
// volatile function is POST-only, otherwise GET/HEAD/POST are allowed too), and
// every table or view answers the full relation verb set. No transaction runs.
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Allow", rootAllow)
	} else if fn, ok := rpcName(r.URL.Path); ok {
		w.Header().Set("Allow", s.rpcAllow(fn))
	} else {
		w.Header().Set("Allow", tableAllow)
	}
	w.WriteHeader(http.StatusOK)
}

// rpcAllow is the Allow value for an OPTIONS on /rpc/<fn>: a volatile function
// accepts only OPTIONS and POST, every other (read-only) function also accepts
// GET and HEAD. A non-registry (native) backend does not resolve volatility
// here, so it answers the read-capable set, matching PostgREST's default for a
// function whose volatility is not yet known.
func (s *Server) rpcAllow(fn string) string {
	const readable = "OPTIONS,GET,HEAD,POST"
	const writeOnly = "OPTIONS,POST"
	if s.backend.Capabilities().NativeRPC {
		return readable
	}
	for _, f := range s.backend.Functions().List() {
		if f.Name == fn && !f.Volatility.ReadOnly() {
			return writeOnly
		}
	}
	return readable
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
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request, fn string, id identity, activeSchema string) {
	if fn == "" || strings.Contains(fn, "/") {
		writeError(w, pgerr.ErrNoFunction(activeSchema, fn, nil, ""))
		return
	}

	isGet := r.Method == http.MethodGet || r.Method == http.MethodHead
	if !isGet && r.Method != http.MethodPost {
		// PUT, PATCH, or DELETE on a function is PostgREST's PGRST101 with the
		// exact "Cannot use the <method> method on RPC" text. OPTIONS never
		// reaches here; it is answered with an Allow header before routing.
		writeError(w, pgerr.ErrInvalidRPCMethod(r.Method))
		return
	}

	media, ok := negotiate(r.Header.Values("Accept"))
	if !ok {
		writeError(w, pgerr.ErrNotAcceptable(strings.Join(r.Header.Values("Accept"), ", ")))
		return
	}

	var body []byte
	if r.Method == http.MethodPost {
		b, apiErr := s.readBody(w, r)
		if apiErr != nil {
			writeError(w, apiErr)
			return
		}
		body = b
	}

	// A portable function with a single unnamed parameter takes the whole POST
	// body as that argument, decoded by Content-Type. The native path discovers
	// its own signatures, so it leaves the unnamed binding to the engine.
	rawBodyParam, rawBodyType := "", ""
	if !isGet && !s.backend.Capabilities().NativeRPC {
		rawBodyParam, rawBodyType = singleRawBodyParam(s.backend.Functions(), fn)
	}

	t := timerFrom(r.Context())

	parseStart := time.Now()
	call, apiErr := ir.ParseCall(fn, r.URL.RawQuery, r.Header.Values("Prefer"), isGet, r.Header.Get("Content-Type"), body, rawBodyParam, rawBodyType)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	t.mark("parse", parseStart)
	call.Singular = singularMedia(media)
	if apiErr := s.applyTxPolicy(&call.Prefer); apiErr != nil {
		writeError(w, apiErr)
		return
	}

	// A GET /rpc read honors the Range header the same way a table read does:
	// it overrides ?limit=&offset= and an inverted range is 416.
	if isGet {
		if rangeHdr := r.Header.Get("Range"); rangeHdr != "" && !strings.Contains(rangeHdr, "=") {
			off, lim, ok, inverted := parseRangeHeader(rangeHdr)
			if inverted {
				writeError(w, pgerr.ErrRangeNotSatisfiable().
					WithDetails("The lower boundary must be lower than or equal to the upper boundary in the Range header."))
				return
			}
			if ok {
				call.Offset = &off
				if lim >= 0 {
					l := lim
					call.Limit = &l
				}
			}
		}
	}

	// db-max-rows caps an RPC response like a read (an implicit LIMIT).
	call.Limit = s.capLimit(call.Limit)

	planStart := time.Now()
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
		planned, apiErr = plan.Call(s.backend.Functions(), call, isGet, []string{activeSchema})
		if apiErr != nil {
			writeError(w, apiErr)
			return
		}
	}
	t.mark("plan", planStart)

	rc := s.buildContext(r, id, activeSchema)
	txStart := time.Now()
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, mapExecError(s.backend, err, id.anonymous))
		return
	}
	t.mark("transaction", txStart)

	respStart := time.Now()
	out, apiErr := renderCall(media, res, planned.Func, fn)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	t.mark("response", respStart)

	s.writeCall(w, r, call, out, res.ResponseControls())
}

// singleRawBodyParam reports the parameter name and type of a function whose
// signature is a single unnamed argument, the form that takes the whole POST
// body as one value decoded by Content-Type. It scans every overload of the
// name and returns the first single-raw-body match; an absent or multi-parameter
// function yields empty strings, leaving the normal named-arguments path. See
// spec 12-rpc and the PostgREST single-unnamed-parameter rule.
func singleRawBodyParam(reg rpc.Registry, name string) (string, string) {
	for _, fn := range reg.List() {
		if fn.Name != name {
			continue
		}
		if p, ok := fn.SingleRawBody(); ok {
			return p.Name, p.Type
		}
	}
	return "", ""
}

// writeCall writes a successful RPC response. An RPC read uses the same
// pagination contract as a table read: Content-Range is always present, an
// out-of-bounds offset is 416, and the 200/206 rule follows the count.
func (s *Server) writeCall(w http.ResponseWriter, r *http.Request, call *ir.Call, out *rendered, ctrl *reqctx.ResponseControls) {
	offset := 0
	if call.Offset != nil {
		offset = *call.Offset
	}
	s.writePaged(w, r, call.Prefer.AppliedHeader(), offset, out, ctrl)
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request, id identity, activeSchema string) {
	relation := strings.Trim(r.URL.Path, "/")
	if relation == "" || strings.Contains(relation, "/") {
		writeError(w, pgerr.ErrUnknownTable(relation))
		return
	}

	t := timerFrom(r.Context())

	acceptHdrs := r.Header.Values("Accept")
	media, ok := negotiate(acceptHdrs)
	if !ok {
		writeError(w, pgerr.ErrNotAcceptable(strings.Join(acceptHdrs, ", ")))
		return
	}

	parseStart := time.Now()
	q, apiErr := ir.ParseRead(relation, r.URL.RawQuery, r.Header.Values("Prefer"))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	t.mark("parse", parseStart)
	q.Singular = singularMedia(media)

	if apiErr := s.applyTxPolicy(&q.Prefer); apiErr != nil {
		writeError(w, apiErr)
		return
	}

	// Range: header overrides ?limit=&offset= and marks the request as a
	// Range request so the server can return 206 Partial Content. PostgREST
	// accepts Range: 0-9 (item range) without requiring Range-Unit: items.
	// Only treat Range as item pagination when it has no unit prefix (i.e.
	// not "bytes=0-9" form), matching PostgREST's parsing behaviour.
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" && !strings.Contains(rangeHdr, "=") {
		off, lim, ok, inverted := parseRangeHeader(rangeHdr)
		if inverted {
			writeError(w, pgerr.ErrRangeNotSatisfiable().
				WithDetails("The lower boundary must be lower than or equal to the upper boundary in the Range header."))
			return
		}
		if ok {
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

	planStart := time.Now()
	planned, apiErr := plan.Read(s.Model(), q, []string{activeSchema}, plan.Options{AggregatesEnabled: s.aggregatesOn})
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	t.mark("plan", planStart)

	rc := s.buildContext(r, id, activeSchema)

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

	txStart := time.Now()
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, mapExecError(s.backend, err, id.anonymous))
		return
	}
	t.mark("transaction", txStart)

	respStart := time.Now()
	out, apiErr := renderFor(media, res, embedKeys(q))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	t.mark("response", respStart)

	s.writeRead(w, r, q, out, res.ResponseControls())
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, kind ir.QueryKind, id identity, activeSchema string) {
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
		b, apiErr := s.readBody(w, r)
		if apiErr != nil {
			writeError(w, apiErr)
			return
		}
		body = b
	}

	t := timerFrom(r.Context())

	parseStart := time.Now()
	q, apiErr := ir.ParseWrite(kind, relation, r.URL.RawQuery, r.Header.Values("Prefer"), r.Header.Get("Content-Type"), body)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	t.mark("parse", parseStart)
	q.Singular = singularMedia(media)

	if apiErr := s.applyTxPolicy(&q.Prefer); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	if q.Write != nil {
		if q.Prefer.Tx != nil {
			q.Write.Tx = *q.Prefer.Tx
		} else {
			q.Write.Tx = ir.TxAuto
		}
	}

	planStart := time.Now()
	planned, apiErr := plan.Write(s.Model(), q, []string{activeSchema})
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	t.mark("plan", planStart)

	rc := s.buildContext(r, id, activeSchema)
	if apiErr := s.authorize(rc, planned); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	txStart := time.Now()
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, mapExecError(s.backend, err, id.anonymous))
		return
	}
	t.mark("transaction", txStart)

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
	// A Location points at a newly created resource. PostgREST sets it only for a
	// return=headers-only POST insert or upsert of a single row; a PUT never
	// carries one (02.9).
	if r.Method == http.MethodPost && (q.Kind == ir.Insert || q.Kind == ir.Upsert) &&
		q.Write != nil && q.Write.Return == ir.ReturnHeadersOnly {
		if loc := locationHeader(rel, q.Relation.Name, res); loc != "" {
			w.Header().Set("Location", loc)
		}
	}

	// Content-Range is present on every write except PUT, shaped by method (02.8):
	// POST and DELETE report the total-only "*/*" form ("*/N" with count=exact),
	// PATCH the affected-row range "0-(n-1)/...". It does not depend on the return
	// mode, so a minimal write carries it too.
	affected, hasAff := res.Affected()
	if cr := writeContentRange(r.Method, affected, hasAff, q.Count); cr != "" {
		w.Header().Set("Content-Range", cr)
	}

	representation := q.Write.Return == ir.ReturnRepresentation
	if !representation {
		w.WriteHeader(applyControls(w, ctrl, writeStatus(r.Method, q.Kind, false, ctrl)))
		return
	}

	respStart := time.Now()
	out, apiErr := renderFor(media, res, embedKeys(q))
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	timerFrom(r.Context()).mark("response", respStart)
	w.Header().Set("Content-Type", out.contentType)
	w.WriteHeader(applyControls(w, ctrl, writeStatus(r.Method, q.Kind, true, ctrl)))
	if r.Method != http.MethodHead {
		w.Write(out.body)
	}
}

// writeContentRange builds the Content-Range header for a write, shaped by the
// HTTP method (02.8). A PUT carries none. POST and DELETE report the total-only
// "*/*" form ("*/N" with count=exact); PATCH reports the affected-row range
// "0-(n-1)/..." and falls back to "*/..." when no row matched.
func writeContentRange(method string, affected int64, hasAff bool, count ir.CountKind) string {
	if method == http.MethodPut {
		return ""
	}
	total := "*"
	if count == ir.CountExact && hasAff {
		total = strconv.FormatInt(affected, 10)
	}
	if method == http.MethodPatch && hasAff && affected > 0 {
		return fmt.Sprintf("0-%d/%s", affected-1, total)
	}
	return "*/" + total
}

// writeStatus is the status for a successful write.
//   - POST insert: 201 Created.
//   - POST merge-duplicates upsert with zero rows inserted: 200 OK.
//   - POST upsert otherwise (ignore-duplicates, mixed, all-insert, unknown): 201.
//   - PUT without representation (minimal, headers-only, none): 204 No Content.
//   - PUT representation with a row inserted: 201 Created; else 200 OK.
//   - PATCH/DELETE with representation: 200 OK; without: 204 No Content.
func writeStatus(method string, kind ir.QueryKind, representation bool, ctrl *reqctx.ResponseControls) int {
	switch method {
	case http.MethodPost:
		// A POST upsert is 200 only when the resolution is merge-duplicates and no
		// row was newly inserted; ignore-duplicates and mixed batches stay 201. The
		// backend reports a known insert count only for a merge upsert, so a known
		// zero here already implies merge-duplicates.
		if kind == ir.Upsert && ctrl != nil && ctrl.UpsertStatusKnown && ctrl.InsertedRows == 0 {
			return http.StatusOK
		}
		return http.StatusCreated
	case http.MethodPut:
		// A PUT answers 204 for every return mode except representation, which is
		// 201 when a row was inserted and 200 when it replaced an existing one.
		if !representation {
			return http.StatusNoContent
		}
		if ctrl != nil && ctrl.UpsertStatusKnown && ctrl.InsertedRows > 0 {
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
	offset := 0
	if q.Offset != nil {
		offset = *q.Offset
	}
	s.writePaged(w, r, q.Prefer.AppliedHeader(), offset, out, ctrl)
}

// writePaged sets the pagination headers and status shared by table reads and
// RPC reads. Content-Range is always present (the "*" total form without a
// count). An offset strictly past a known total is 416 with the upstream
// detail; an offset equal to the total is in range and yields 206 with
// Content-Range "*/total". The 200/206 rule is 206 whenever a total is known
// and the returned span is smaller, for every count kind (PostgREST v14 returns
// 206 for count=planned/estimated too). A function or policy can override the
// status and add headers through the controls.
func (s *Server) writePaged(w http.ResponseWriter, r *http.Request, applied string, offset int, out *rendered, ctrl *reqctx.ResponseControls) {
	if applied != "" {
		w.Header().Set("Preference-Applied", applied)
	}
	w.Header().Set("Content-Type", out.contentType)
	w.Header().Set("Content-Range", contentRange(offset, out.nRows, out.total, out.hasTotl))

	if offset > 0 && out.hasTotl && int64(offset) > out.total {
		rng := pgerr.ErrRangeNotSatisfiable().
			WithDetails(fmt.Sprintf("An offset of %d was requested, but there are only %d rows.", offset, out.total))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(rng.HTTPStatus)
		if r.Method != http.MethodHead {
			w.Write(rng.JSON())
		}
		return
	}

	status := http.StatusOK
	if out.hasTotl && int64(out.nRows) < out.total {
		status = http.StatusPartialContent
	}
	w.WriteHeader(applyControls(w, ctrl, status))
	if r.Method != http.MethodHead {
		w.Write(out.body)
	}
}

// parseRangeHeader parses an HTTP Range header value of the form "start-end"
// (as used with Range-Unit: items). Returns (offset, limit, true) where limit
// is -1 for an open-ended range ("0-"). A malformed header returns ok=false with
// inverted=false so the caller serves the full result. A well-formed header whose
// upper bound is below its lower bound returns ok=false with inverted=true, which
// PostgREST answers with 416 rather than ignoring.
func parseRangeHeader(s string) (offset, limit int, ok, inverted bool) {
	dash := strings.LastIndex(s, "-")
	if dash < 0 {
		return 0, 0, false, false
	}
	startStr, endStr := s[:dash], s[dash+1:]
	start, err := strconv.Atoi(startStr)
	if err != nil || start < 0 {
		return 0, 0, false, false
	}
	if endStr == "" {
		return start, -1, true, false // open-ended: "0-"
	}
	end, err := strconv.Atoi(endStr)
	if err != nil {
		return 0, 0, false, false
	}
	if end < start {
		return 0, 0, false, true
	}
	return start, end - start + 1, true, false
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
	// PostgREST does not echo Content-Profile on an error response; drop the
	// header ServeHTTP may have staged before the handler failed.
	w.Header().Del("Content-Profile")
	e.Write(w)
}
