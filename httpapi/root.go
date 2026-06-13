package httpapi

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/tamnd/dbrest/config"
	"github.com/tamnd/dbrest/ir"
	"github.com/tamnd/dbrest/openapi"
	"github.com/tamnd/dbrest/pgerr"
	"github.com/tamnd/dbrest/plan"
	"github.com/tamnd/dbrest/reqctx"
	"github.com/tamnd/dbrest/schema"
)

// handleRoot serves the self-describing OpenAPI document at GET /. The document
// is generated from the current schema model, the RPC registry, and the
// backend's declared capabilities, so it describes exactly what this server can
// serve and never promises an operator the next request would reject. HEAD
// returns the headers with no body. See spec 19.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request, id identity, activeSchema string) {
	if r.Method == http.MethodOptions {
		// OPTIONS on the root answers with the verb set, the way PostgREST's
		// info response does, with no body and no media type.
		w.Header().Set("Allow", rootAllow)
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", rootAllow)
		writeError(w, pgerr.ErrUnsupportedMethod(r.Method))
		return
	}
	if s.openapiMode == config.OpenAPIDisabled {
		// openapi-mode=disabled turns the self-describing root off entirely; a
		// request there is PostgREST's 404 PGRST126.
		writeError(w, errRootDisabled())
		return
	}
	if !rootAcceptable(r.Header.Values("Accept")) {
		writeError(w, pgerr.ErrNotAcceptable(acceptedList(r.Header.Values("Accept"))))
		return
	}
	if s.rootSpec != "" {
		// db-root-spec replaces the generated document with the named
		// function's result, upstream's escape hatch for a custom spec.
		s.serveRootSpec(w, r, id, activeSchema)
		return
	}

	opts := openapi.Options{
		Host:           r.Host,
		Schemes:        []string{requestScheme(r)},
		SecurityActive: s.openapiSecurity,
		ActiveSchema:   activeSchema,
	}
	model := s.Model()
	if comment := model.SchemaComment(activeSchema); comment != "" {
		// The database comment on the active schema names the API: the first
		// line is the info title, the rest the description, as v14 reads it.
		title, rest, _ := strings.Cut(comment, "\n")
		opts.Title = title
		opts.Description = strings.TrimSpace(rest)
	}
	if s.openapiProxy != "" {
		applyProxyURI(&opts, s.openapiProxy)
	}
	if s.openapiMode == config.OpenAPIFollowPrivileges && s.authz != nil {
		// follow-privileges scopes the document to what the requesting role may
		// actually do, so an anonymous caller cannot enumerate relations it
		// cannot touch. The answers come from the same gate that authorizes a
		// real request; ignore-privileges leaves Visibility nil and emits all.
		rc := s.buildContext(r, id, activeSchema)
		opts.Visibility = func(rel *schema.Relation) openapi.Actions {
			return openapi.Actions{
				Get:    s.probeAction(rc, rel.Name, ir.Read),
				Post:   s.probeAction(rc, rel.Name, ir.Insert),
				Patch:  s.probeAction(rc, rel.Name, ir.Update),
				Delete: s.probeAction(rc, rel.Name, ir.Delete),
			}
		}
	}
	body, err := openapi.Generate(model, s.backend.Functions(), s.backend.Capabilities(), opts)
	if err != nil {
		writeError(w, pgerr.ErrInternal(err.Error()))
		return
	}

	w.Header().Set("Content-Type", openapi.MediaType+"; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// serveRootSpec invokes the db-root-spec function and serves its JSON result
// in place of the generated document. The call runs exactly like GET
// /rpc/<fn> with no arguments, the same planning and execution path, so role
// switching and error mapping behave identically; only the response media
// type differs, staying the root's openapi+json.
func (s *Server) serveRootSpec(w http.ResponseWriter, r *http.Request, id identity, activeSchema string) {
	call, apiErr := ir.ParseCall(s.rootSpec, "", nil, true, "", nil, "", "")
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	var planned *ir.Plan
	if s.backend.Capabilities().NativeRPC {
		planned = &ir.Plan{Call: call, ReadOnly: true}
	} else {
		planned, apiErr = plan.Call(s.backend.Functions(), s.Model(), call, true, []string{activeSchema})
		if apiErr != nil {
			writeError(w, apiErr)
			return
		}
	}

	rc := s.buildContext(r, id, activeSchema)
	res, err := s.backend.Execute(r.Context(), planned, rc)
	if err != nil {
		writeError(w, mapExecError(s.backend, err, id.anonymous))
		return
	}
	out, apiErr := renderCall(mediaJSON, res, planned.Func, s.rootSpec)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}

	w.Header().Set("Content-Type", openapi.MediaType+"; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(out.body)
	}
}

// rootAllow is the verb set the root serves: the Allow value OPTIONS answers
// with and the one a 405 carries so the rejected caller knows what would work.
const rootAllow = "OPTIONS,GET,HEAD"

// errRootDisabled is PostgREST's PGRST126: the root metadata endpoint turned
// off by openapi-mode=disabled (or an unset db-root-spec in that mode), a 404
// with an explicit code rather than a bare not-found.
func errRootDisabled() *pgerr.APIError {
	return pgerr.New(http.StatusNotFound, "PGRST126", "Root endpoint metadata is disabled")
}

// rootAcceptable reports whether the Accept header admits the root document.
// The root produces application/openapi+json and application/json only; an
// absent header or a wildcard range accepts it, anything else is the caller's
// 406 PGRST107 (PostgREST root negotiation).
func rootAcceptable(accept []string) bool {
	ranges := parseAccept(accept)
	if len(ranges) == 0 {
		return true
	}
	for _, mr := range ranges {
		if mr.q <= 0 {
			continue
		}
		if mr.typ == "*" && mr.sub == "*" {
			return true
		}
		if mr.typ == "application" && (mr.sub == "*" || mr.sub == "openapi+json" || mr.sub == "json") {
			return true
		}
	}
	return false
}

// acceptedList renders the requested media types for the PGRST107 message the
// way PostgREST does: parameters stripped, ordered by descending quality.
func acceptedList(accept []string) string {
	ranges := parseAccept(accept)
	parts := make([]string, len(ranges))
	for i, mr := range ranges {
		parts[i] = mr.typ + "/" + mr.sub
	}
	return strings.Join(parts, ", ")
}

// probeAction asks the authorization gate whether the role could perform one
// kind of query on a relation, by authorizing a minimal throwaway plan. Using
// the real gate keeps the document's answer identical to what a request would
// get; the probe plan is discarded, so the gate's mutations never escape.
func (s *Server) probeAction(rc *reqctx.Context, rel string, kind ir.QueryKind) bool {
	q := &ir.Query{Kind: kind, Relation: ir.Ref{Name: rel}}
	if kind != ir.Read {
		q.Write = &ir.WriteSpec{}
	}
	return s.authz.Authorize(rc, &ir.Plan{Query: q}) == nil
}

// requestScheme reports the URL scheme the client reached the server with,
// reading the TLS state. Behind a proxy this is the listen-side scheme; the
// externally visible scheme comes from the proxy-uri configuration (spec 20).
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// applyProxyURI overrides the document's host, scheme, and base path with the
// parts of the configured externally visible URL. A path of "/" or empty is
// left as the generator default. A URL that does not parse is ignored, so a
// misconfigured proxy-uri degrades to the request-derived values rather than
// breaking the root.
func applyProxyURI(opts *openapi.Options, proxy string) {
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return
	}
	opts.Host = u.Host
	if u.Scheme != "" {
		opts.Schemes = []string{u.Scheme}
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		opts.BasePath = p
	}
}
