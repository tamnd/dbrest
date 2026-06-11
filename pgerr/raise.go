package pgerr

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// A function can take full control of the response by raising SQLSTATE 'PGRST'
// with a JSON object in MESSAGE ({code, message, details?, hint?}, the
// envelope) and a JSON object in DETAIL ({status, headers, status_text?}, the
// response control). PostgREST forwards the envelope verbatim, sets the HTTP
// status from detail.status, and applies detail.headers; a payload it cannot
// parse is reported as PGRST121 at 500 with details naming the malformed
// field. All texts and the obligatory-key rules below were verified against a
// live v14 (code and message are obligatory in MESSAGE; status and headers,
// which may be an empty object, are obligatory in DETAIL).

// CodeRaiseParse is PGRST121: the MESSAGE or DETAIL payload of a RAISE
// SQLSTATE 'PGRST' could not be parsed.
const CodeRaiseParse = "PGRST121"

const (
	raiseParseMessage = `Could not parse JSON in the "RAISE SQLSTATE 'PGRST'" error`
	raiseMessageHint  = "MESSAGE must be a JSON object with obligatory keys: 'code', 'message' and optional keys: 'details', 'hint'."
	raiseDetailHint   = "DETAIL must be a JSON object with obligatory keys: 'status', 'headers' and optional key: 'status_text'."
)

// ErrRaiseParse is the PGRST121 envelope, with details naming the malformed
// field and the hint spelling the expected shape.
func ErrRaiseParse(details, hint string) *APIError {
	return New(http.StatusInternalServerError, CodeRaiseParse, raiseParseMessage).
		WithDetails(details).WithHint(hint)
}

// raiseMessage is the envelope object a function puts in MESSAGE. Pointer
// fields distinguish a missing obligatory key from an empty value.
type raiseMessage struct {
	Code    *string `json:"code"`
	Message *string `json:"message"`
	Details *string `json:"details"`
	Hint    *string `json:"hint"`
}

// raiseDetail is the response-control object a function puts in DETAIL.
type raiseDetail struct {
	Status     *int              `json:"status"`
	StatusText *string           `json:"status_text"`
	Headers    map[string]string `json:"headers"`
}

// FromRaise assembles the client-controlled error from the MESSAGE and DETAIL
// strings of a RAISE SQLSTATE 'PGRST'. On success it returns the function's
// envelope with the status from detail.status, plus the headers to apply to
// the response. When either payload cannot be parsed it returns the PGRST121
// envelope and no headers, exactly as PostgREST does; pass detail as the empty
// string when the RAISE carried no DETAIL.
func FromRaise(message, detail string) (*APIError, map[string]string) {
	var m raiseMessage
	if err := json.Unmarshal([]byte(message), &m); err != nil || m.Code == nil || m.Message == nil {
		return ErrRaiseParse(
			fmt.Sprintf("Invalid JSON value for MESSAGE: '%s'", message), raiseMessageHint), nil
	}
	if detail == "" {
		return ErrRaiseParse("DETAIL is missing in the RAISE statement", raiseDetailHint), nil
	}
	var d raiseDetail
	if err := json.Unmarshal([]byte(detail), &d); err != nil || d.Status == nil || d.Headers == nil {
		return ErrRaiseParse(
			fmt.Sprintf("Invalid JSON value for DETAIL: '%s'", detail), raiseDetailHint), nil
	}
	e := New(*d.Status, *m.Code, *m.Message)
	e.Details = m.Details
	e.Hint = m.Hint
	return e, d.Headers
}
