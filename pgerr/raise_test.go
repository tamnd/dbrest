package pgerr

import (
	"net/http"
	"testing"
)

// The happy path: a function controls status, headers, and the whole envelope.
// The payloads mirror the documented example, which a live v14 answers with
// 402, the X-Powered-By header, and the envelope verbatim.
func TestFromRaiseFullControl(t *testing.T) {
	e, headers := FromRaise(
		`{"code":"123","message":"Payment Required","details":"Quota exceeded","hint":"Upgrade your plan"}`,
		`{"status":402,"headers":{"X-Powered-By":"Nerd Rage"}}`)
	if e.HTTPStatus != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", e.HTTPStatus)
	}
	if e.Code != "123" || e.Message != "Payment Required" {
		t.Errorf("envelope = %s: %s", e.Code, e.Message)
	}
	if e.Details == nil || *e.Details != "Quota exceeded" {
		t.Errorf("details = %v", e.Details)
	}
	if e.Hint == nil || *e.Hint != "Upgrade your plan" {
		t.Errorf("hint = %v", e.Hint)
	}
	if headers["X-Powered-By"] != "Nerd Rage" {
		t.Errorf("headers = %v", headers)
	}
}

// details and hint are optional in MESSAGE; headers may be an empty object.
func TestFromRaiseMinimal(t *testing.T) {
	e, headers := FromRaise(`{"code":"123","message":"m"}`, `{"status":402,"headers":{}}`)
	if e.Code != "123" || e.HTTPStatus != 402 {
		t.Errorf("envelope = %s status %d", e.Code, e.HTTPStatus)
	}
	if e.Details != nil || e.Hint != nil {
		t.Errorf("details/hint should stay null: %v %v", e.Details, e.Hint)
	}
	if headers == nil || len(headers) != 0 {
		t.Errorf("headers = %v, want empty map", headers)
	}
}

// Every malformed payload comes back as the PGRST121 envelope with details
// naming the field and the hint spelling the expected shape; the texts are
// pinned to a live v14's byte for byte.
func TestFromRaiseParseFailures(t *testing.T) {
	cases := []struct {
		name            string
		message, detail string
		details, hint   string
	}{
		{"message not json", "not json", `{"status":402,"headers":{}}`,
			"Invalid JSON value for MESSAGE: 'not json'", raiseMessageHint},
		{"message missing code", `{"message":"no code"}`, `{"status":419,"headers":{}}`,
			`Invalid JSON value for MESSAGE: '{"message":"no code"}'`, raiseMessageHint},
		{"detail not json", `{"code":"123","message":"ok"}`, "nope",
			"Invalid JSON value for DETAIL: 'nope'", raiseDetailHint},
		{"detail missing headers", `{"code":"123","message":"m"}`, `{"status":402}`,
			`Invalid JSON value for DETAIL: '{"status":402}'`, raiseDetailHint},
		{"detail missing", `{"code":"123","message":"just msg"}`, "",
			"DETAIL is missing in the RAISE statement", raiseDetailHint},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, headers := FromRaise(c.message, c.detail)
			if headers != nil {
				t.Errorf("headers = %v, want none on a parse failure", headers)
			}
			if e.HTTPStatus != http.StatusInternalServerError || e.Code != CodeRaiseParse {
				t.Errorf("got %d %s, want 500 PGRST121", e.HTTPStatus, e.Code)
			}
			if e.Message != raiseParseMessage {
				t.Errorf("message = %q", e.Message)
			}
			if e.Details == nil || *e.Details != c.details {
				t.Errorf("details = %v, want %q", e.Details, c.details)
			}
			if e.Hint == nil || *e.Hint != c.hint {
				t.Errorf("hint = %v, want %q", e.Hint, c.hint)
			}
		})
	}
}
