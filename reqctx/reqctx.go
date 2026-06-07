// Package reqctx carries the per-request context the frontend builds and hands
// to a backend: the resolved role, JWT claims, request metadata, and the
// response controls a backend can write back (status and header overrides).
//
// The frontend always builds the context regardless of backend; on PostgreSQL
// the role and claims are later pushed to the engine as GUCs, on the emulated
// backends they drive the authz layer. See spec 13/15.
package reqctx

// Context is the request context passed to Backend.Execute.
type Context struct {
	// Role is the resolved request role (web_user, anon, ...).
	Role string
	// Claims are the verified JWT claims, or nil for an anonymous request.
	Claims map[string]any
	// Method is the HTTP method.
	Method string
	// Path is the request path.
	Path string
	// Headers is a read-only view of request headers the backend may consult.
	Headers map[string][]string

	controls ResponseControls
}

// ResponseControls are status and header overrides a backend reads back after
// Execute, for features like Location on insert or a function-set status.
type ResponseControls struct {
	// Status, when non-zero, overrides the default response status.
	Status int
	// Headers are extra response headers to merge in.
	Headers map[string]string
}

// Controls returns a pointer to the mutable response controls.
func (c *Context) Controls() *ResponseControls { return &c.controls }

// SetHeader records a response header override.
func (rc *ResponseControls) SetHeader(k, v string) {
	if rc.Headers == nil {
		rc.Headers = make(map[string]string)
	}
	rc.Headers[k] = v
}
