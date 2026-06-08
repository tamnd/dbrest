package httpapi

import (
	"net/http"

	"github.com/tamnd/dbrest/openapi"
	"github.com/tamnd/dbrest/pgerr"
)

// handleRoot serves the self-describing OpenAPI document at GET /. The document
// is generated from the current schema model, the RPC registry, and the
// backend's declared capabilities, so it describes exactly what this server can
// serve and never promises an operator the next request would reject. HEAD
// returns the headers with no body. See spec 19.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, pgerr.ErrUnsupported(r.Method+" requests on the root", "dbrest"))
		return
	}

	opts := openapi.Options{
		Host:    r.Host,
		Schemes: []string{requestScheme(r)},
		JWT:     s.verifier != nil,
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
