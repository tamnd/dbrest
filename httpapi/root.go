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

	w.Header().Set("Content-Type", openapi.MediaType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
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
