// Package reqctx carries the per-request context the frontend builds and hands
// to a backend: the resolved role, JWT claims, request metadata, and the
// response controls a backend can write back (status and header overrides).
//
// The frontend always builds the context regardless of backend. On PostgreSQL
// the role, claims, headers, cookies, method, and path are pushed to the engine
// as GUCs (request.jwt.claims, request.headers, ...) with set_config, so SQL and
// RLS predicates can read them with current_setting; on SQL Server the analog is
// SESSION_CONTEXT. On the emulated backends (SQLite here) there is no setting a
// query can read mid-statement, so the values are kept in this struct and the
// specific ones a policy references are bound as parameters when the predicate is
// injected into the IR (spec 14). Either way the frontend's view is the same:
// build the context, call Execute, then apply the response controls at render
// time. See spec 13/15.
package reqctx

import (
	"encoding/json"
	"sort"
	"strings"
)

// Context is the request context passed to Backend.Execute.
type Context struct {
	// Role is the resolved request role (web_user, anon, ...).
	Role string
	// Anonymous reports that the request carried no usable JWT and runs as the
	// anon role. It selects 401 over 403 when authorization denies the request:
	// an unauthenticated caller gets 401, an authenticated one 403 (spec 14).
	Anonymous bool
	// Claims are the verified JWT claims, or nil for an anonymous request.
	Claims map[string]any
	// Method is the HTTP method.
	Method string
	// Path is the request path.
	Path string
	// Headers is a read-only view of request headers the backend may consult. It
	// is the raw multi-valued form; HeadersJSON flattens it to the GUC shape.
	Headers map[string][]string
	// Cookies are the request cookies by name, the source of request.cookies.
	Cookies map[string]string
	// Schema is the selected schema for the request (the Accept-Profile or
	// Content-Profile choice), or "" for the default. Cross-schema routing is the
	// introspection subsystem's job (spec 08); this field carries the choice.
	Schema string
	// PreRequest names the db-pre-request function the backend must invoke after
	// the request context is in place and before the main statement, in the same
	// transaction (spec 13). Empty means none is configured. An error the
	// function raises aborts the request through normal error mapping, and any
	// response controls it writes are applied at render time.
	PreRequest string

	// AppSettings are the app.settings.* options, keys without the prefix. A
	// backend applies them as transaction settings (GUCs on PostgreSQL) next
	// to the request context.
	AppSettings map[string]string

	// LogQuery asks the backend to echo the statements it executes for this
	// request, the log-query option.
	LogQuery bool

	controls ResponseControls
}

// ClaimsJSON marshals the verified claims into the object request.jwt.claims
// carries. It is "{}" when there are no claims, never null, so a backend that
// writes the GUC verbatim and a policy that reads it both see a valid object.
func (c *Context) ClaimsJSON() []byte {
	if len(c.Claims) == 0 {
		return []byte("{}")
	}
	// encoding/json sorts map keys, so the output is deterministic.
	b, err := json.Marshal(c.Claims)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// HeadersJSON marshals the request headers into the object request.headers
// carries: a JSON object of lower-cased header name to value, with a multi-valued
// header joined by ", " as HTTP defines. Keys are sorted for a deterministic
// document.
func (c *Context) HeadersJSON() []byte {
	flat := make(map[string]string, len(c.Headers))
	for k, vs := range c.Headers {
		flat[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	return marshalSortedObject(flat)
}

// CookiesJSON marshals the request cookies into the object request.cookies
// carries: a JSON object of cookie name to value, keys sorted.
func (c *Context) CookiesJSON() []byte {
	return marshalSortedObject(c.Cookies)
}

// marshalSortedObject renders a string map as a JSON object with sorted keys, so
// the GUC documents are byte-stable across requests and backends.
func marshalSortedObject(m map[string]string) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// ResponseControls are status and header overrides a backend reads back after
// Execute, for features like Location on insert or a function-set status. A
// function or policy writes them (a set_config('response.status', ...) round
// trip on PostgreSQL, a direct write on the emulated backends) and the renderer
// applies them: a non-zero Status overrides the default, and each header is set.
type ResponseControls struct {
	// Status, when non-zero, overrides the default response status.
	Status int
	// Headers are extra response headers to merge in.
	Headers map[string]string
	// InsertedRows is the number of payload rows the upsert inserted as new (the
	// rest replaced existing rows). The HTTP layer reads it to separate a
	// zero-inserted merge from a mixed batch: a POST merge upsert is 200 only when
	// it is zero, a PUT is 201 only when it is positive. A backend sets it together
	// with UpsertStatusKnown = true when it can detect insert-vs-update (sqlite by a
	// pre-write key probe, PostgreSQL via xmax); others leave the status unknown and
	// the HTTP layer defaults to 201 for POST upserts.
	UpsertStatusKnown bool
	InsertedRows      int
}

// Controls returns a pointer to the mutable response controls.
func (c *Context) Controls() *ResponseControls { return &c.controls }

// SetStatus records a response status override.
func (rc *ResponseControls) SetStatus(status int) { rc.Status = status }

// SetHeader records a response header override.
func (rc *ResponseControls) SetHeader(k, v string) {
	if rc.Headers == nil {
		rc.Headers = make(map[string]string)
	}
	rc.Headers[k] = v
}
