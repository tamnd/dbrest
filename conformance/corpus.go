// Package conformance is the differential test harness that defines what
// "PostgREST-compatible" means as a runnable artifact rather than a claim. It
// replays one neutral request corpus against a subject (an in-process dbrest
// server) and a golden reference, normalizes both responses, and reports each
// request as golden-equal, allowlisted, or a failure. It also turns the
// capability matrix into assertions: a Native or Emulated feature must produce
// an equivalent response, and an Unsupported one must return PGRST127 rather
// than a wrong answer. See spec 22.
//
// The golden side is a real PostgREST in front of PostgreSQL at pinned
// versions; here it is supplied as a recorded corpus (the on-disk form the
// captured golden responses take), so the normalization, comparison, allowlist,
// matrix, and capability-consistency machinery all run today against the
// in-process SQLite subject with no external services. The live capture job
// lands with the container CI matrix (spec 22 section 8).
package conformance

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
)

// Request is one neutral request in the corpus, independent of any engine: a
// method, a path, a query string, headers, and an optional body. The same
// Request addresses the golden side and every subject backend (spec 22).
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// Target is the request line the replay runner issues, joining the path and the
// query string.
func (r Request) Target() string {
	if r.Query == "" {
		return r.Path
	}
	return r.Path + "?" + r.Query
}

// Ordered reports whether the request pins row order with an order parameter.
// Without it, row order is not contractual on any engine, so the comparison
// treats the result array as a set (spec 22 section 2).
func (r Request) Ordered() bool {
	if r.Query == "" {
		return false
	}
	vals, err := url.ParseQuery(r.Query)
	if err != nil {
		return false
	}
	return vals.Has("order")
}

// Response is a captured HTTP response: the status code, the headers, and the
// raw body. The golden response is the expected value; a subject response is
// compared to it after normalization.
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// Case pairs a request with the golden response it must reproduce, plus the
// optional feature label that ties it to a capability tier and an allowlist
// entry. Mask lists JSON pointers into the body whose values are volatile and
// are blanked before comparison (timestamps, generated ids).
type Case struct {
	Name    string   `json:"name"`
	Feature string   `json:"feature,omitempty"`
	Request Request  `json:"request"`
	Golden  Response `json:"golden"`
	Mask    []string `json:"mask,omitempty"`
}

// LoadCorpus reads a JSON array of cases from path.
func LoadCorpus(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("conformance: reading corpus %s: %w", path, err)
	}
	var cases []Case
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, fmt.Errorf("conformance: parsing corpus %s: %w", path, err)
	}
	for i := range cases {
		if cases[i].Name == "" {
			return nil, fmt.Errorf("conformance: corpus %s: case %d has no name", path, i)
		}
		if cases[i].Request.Method == "" {
			return nil, fmt.Errorf("conformance: corpus %s: case %q has no method", path, cases[i].Name)
		}
	}
	return cases, nil
}
