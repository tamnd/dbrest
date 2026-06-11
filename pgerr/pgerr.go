// Package pgerr defines the unified error envelope and the PGRST code table.
//
// Every error dbrest returns to a client is rendered by exactly one serializer
// here, fed by per-stage and per-backend mappers that normalize onto a single
// APIError value. This is what makes the error body byte-identical across
// engines: there is one renderer, not one per backend. See spec 18-errors.md.
package pgerr

import (
	"encoding/json"
	"net/http"
)

// APIError is the canonical error value. It carries the wire envelope
// (code, message, details, hint) plus the HTTP status used to render it.
//
// The JSON body is the PostgREST shape: the four keys are always present, with
// details and hint serialized as null when empty. No backend writes an error
// body; a backend's only job is to map its native error onto an *APIError.
type APIError struct {
	// HTTPStatus is the HTTP status code. It is not part of the JSON body.
	HTTPStatus int `json:"-"`
	// Code is the PGRST code (or a backend SQLSTATE passed through).
	Code string `json:"code"`
	// Message is the human-facing summary.
	Message string `json:"message"`
	// Details is extra context, or null.
	Details *string `json:"details"`
	// RawDetails carries a details payload that is not a string: PostgREST's
	// PGRST201 returns details as a JSON array of candidate relationship
	// objects, which clients read to auto-disambiguate an embed. When set it
	// takes precedence over Details in the rendered envelope.
	RawDetails json.RawMessage `json:"-"`
	// Hint is a suggested fix, or null.
	Hint *string `json:"hint"`
}

// Error implements the error interface.
func (e *APIError) Error() string {
	if e == nil {
		return "<nil pgerr.APIError>"
	}
	return e.Code + ": " + e.Message
}

// WithDetails returns a copy of e with details set.
func (e *APIError) WithDetails(details string) *APIError {
	c := *e
	c.Details = &details
	return &c
}

// WithDetailsJSON returns a copy of e with details set to a non-string JSON
// value, the shape PGRST201 uses for its candidate relationship array. v is
// marshaled immediately; a value that cannot marshal leaves details unchanged
// rather than corrupting the envelope.
func (e *APIError) WithDetailsJSON(v any) *APIError {
	c := *e
	if b, err := json.Marshal(v); err == nil {
		c.RawDetails = b
	}
	return &c
}

// WithHint returns a copy of e with hint set.
func (e *APIError) WithHint(hint string) *APIError {
	c := *e
	c.Hint = &hint
	return &c
}

// WithMessage returns a copy of e with the message replaced.
func (e *APIError) WithMessage(msg string) *APIError {
	c := *e
	c.Message = msg
	return &c
}

// body is the exact JSON shape sent to the client. Keys are always present;
// details and hint are encoded as null when unset. details is raw so it can be
// a string, null, or PGRST201's array of relationship candidates.
type body struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
	Hint    *string         `json:"hint"`
}

// JSON returns the rendered envelope bytes for e.
func (e *APIError) JSON() []byte {
	details := json.RawMessage("null")
	switch {
	case e.RawDetails != nil:
		details = e.RawDetails
	case e.Details != nil:
		if b, err := json.Marshal(*e.Details); err == nil {
			details = b
		}
	}
	b, _ := json.Marshal(body{
		Code:    e.Code,
		Message: e.Message,
		Details: details,
		Hint:    e.Hint,
	})
	return b
}

// Write renders e onto w: it sets the JSON content type, the Proxy-Status
// header, and the status, then writes the envelope. It is the single place an
// error reaches the client. v14 adds Proxy-Status to every error response so a
// HEAD request, whose status alone is not descriptive enough, still names the
// error code; the "PostgREST" identifier is kept byte-identical for wire
// compatibility.
func (e *APIError) Write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Proxy-Status", "PostgREST; error="+e.Code)
	w.WriteHeader(e.HTTPStatus)
	_, _ = w.Write(e.JSON())
}

// New builds an APIError from its parts.
func New(status int, code, message string) *APIError {
	return &APIError{HTTPStatus: status, Code: code, Message: message}
}

// As extracts an *APIError from err if it is one, returning nil otherwise.
func As(err error) *APIError {
	if err == nil {
		return nil
	}
	if e, ok := err.(*APIError); ok {
		return e
	}
	return nil
}
