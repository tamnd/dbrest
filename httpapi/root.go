package httpapi

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/tamnd/dbrest/config"
	"github.com/tamnd/dbrest/openapi"
	"github.com/tamnd/dbrest/pgerr"
)

// handleRoot serves the self-describing OpenAPI document at GET /. The document
// is generated from the current schema model, the RPC registry, and the
// backend's declared capabilities, so it describes exactly what this server can
// serve and never promises an operator the next request would reject. HEAD
// returns the headers with no body. See spec 19.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request, activeSchema string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, pgerr.ErrUnsupported(r.Method+" requests on the root", "dbrest"))
		return
	}
	if s.openapiMode == config.OpenAPIDisabled {
		// openapi-mode=disabled turns the self-describing root off entirely; a
		// request there is a plain 404, as PostgREST returns.
		writeError(w, pgerr.New(http.StatusNotFound, "", "openapi is disabled"))
		return
	}
	if !rootAcceptable(r.Header.Values("Accept")) {
		writeError(w, pgerr.ErrNotAcceptable(acceptedList(r.Header.Values("Accept"))))
		return
	}

	opts := openapi.Options{
		Host:         r.Host,
		Schemes:      []string{requestScheme(r)},
		JWT:          s.verifier != nil,
		ActiveSchema: activeSchema,
	}
	if s.openapiProxy != "" {
		applyProxyURI(&opts, s.openapiProxy)
	}
	body, err := openapi.Generate(s.model, s.backend.Functions(), s.backend.Capabilities(), opts)
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
