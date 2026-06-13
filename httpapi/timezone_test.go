package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// 02.3: Prefer: timezone= sets the request timezone. A valid IANA zone is
// honored and echoed in Preference-Applied; an invalid zone is ignored under
// lenient handling and a PGRST122 violation under handling=strict. The
// engine-agnostic parse/validate/echo is exercised here against sqlite; the
// SET LOCAL timezone effect on temporal output is a live-postgres concern.

// TestTimeZoneEchoed: a GET carrying a valid timezone echoes it back.
func TestTimeZoneEchoed(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=id", map[string]string{
		"Prefer": "timezone=America/Los_Angeles",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if pa := resp.Header.Get("Preference-Applied"); !strings.Contains(pa, "timezone=America/Los_Angeles") {
		t.Errorf("Preference-Applied = %q, want the timezone echoed", pa)
	}
}

// TestTimeZoneInvalidLenientIgnored: an unknown zone under the default lenient
// handling is dropped, not echoed, and the request still succeeds.
func TestTimeZoneInvalidLenientIgnored(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=id", map[string]string{
		"Prefer": "timezone=Mars/Phobos",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if pa := resp.Header.Get("Preference-Applied"); strings.Contains(pa, "timezone") {
		t.Errorf("Preference-Applied = %q, want no timezone echo for an invalid zone", pa)
	}
}

// TestTimeZoneInvalidStrictRejected: an unknown zone under handling=strict is a
// 400 PGRST122 preference violation.
func TestTimeZoneInvalidStrictRejected(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=id", map[string]string{
		"Prefer": "handling=strict, timezone=Mars/Phobos",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env["code"] != "PGRST122" {
		t.Errorf("code = %v, want PGRST122", env["code"])
	}
}
