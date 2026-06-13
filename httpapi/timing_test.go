package httpapi_test

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestServerTimingAbsentByDefault checks dbrest matches a default PostgREST: no
// Server-Timing header until server-timing-enabled is set.
func TestServerTimingAbsentByDefault(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?order=id", nil)
	if got := resp.Header.Get("Server-Timing"); got != "" {
		t.Errorf("Server-Timing = %q, want absent", got)
	}
}

// TestServerTimingEnabledOnRead checks an enabled server emits the documented
// phase names on a read.
func TestServerTimingEnabledOnRead(t *testing.T) {
	srv := newServer(t)
	srv.SetServerTimingEnabled(true)
	resp := do(t, srv, http.MethodGet, "/films?order=id", nil)
	got := resp.Header.Get("Server-Timing")
	if got == "" {
		t.Fatal("Server-Timing header missing")
	}
	for _, phase := range []string{"jwt", "parse", "plan", "transaction", "response"} {
		if !strings.Contains(got, phase+";dur=") {
			t.Errorf("Server-Timing %q missing phase %q", got, phase)
		}
	}
	// Every phase carries a numeric millisecond duration.
	if !regexp.MustCompile(`dur=\d`).MatchString(got) {
		t.Errorf("Server-Timing %q has no numeric durations", got)
	}
}

// TestServerTimingEnabledOnError checks the header is present even when the
// request fails before a transaction, since the wrapper emits it on every exit.
func TestServerTimingEnabledOnError(t *testing.T) {
	srv := newServer(t)
	srv.SetServerTimingEnabled(true)
	resp := do(t, srv, http.MethodGet, "/nonesuch?order=id", nil)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected an error status for an unknown table")
	}
	if got := resp.Header.Get("Server-Timing"); got == "" || !strings.Contains(got, "jwt;dur=") {
		t.Errorf("Server-Timing on error = %q, want a jwt phase", got)
	}
}
