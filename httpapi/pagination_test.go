package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestReadPlannedCountReturns206 checks a bounded window under a non-exact count
// is 206, not 200: PostgREST returns 206 whenever a total is known and the span
// is smaller, for every count kind. SQLite downgrades planned to an exact total,
// so the four-row table over a one-row window is a genuine partial.
func TestReadPlannedCountReturns206(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?limit=1&order=id", map[string]string{
		"Prefer": "count=planned",
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-0/4" {
		t.Errorf("Content-Range = %q, want 0-0/4", cr)
	}
}

// TestReadOffsetEqualsTotalIs206 checks an offset equal to the total is in range:
// zero rows with 206 and Content-Range "*/total", the case a paginate-until-empty
// loop lands on when the total is an exact multiple of the page size.
func TestReadOffsetEqualsTotalIs206(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?offset=4&order=id", map[string]string{
		"Prefer": "count=exact",
	})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "*/4" {
		t.Errorf("Content-Range = %q, want */4", cr)
	}
}

// TestReadOffsetBeyondTotalIs416 checks an offset strictly past the end is still
// 416, the boundary one row beyond the equal-to-total case.
func TestReadOffsetBeyondTotalIs416(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?offset=5&order=id", map[string]string{
		"Prefer": "count=exact",
	})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}
	var env struct{ Code, Details string }
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "PGRST103" {
		t.Errorf("code = %q, want PGRST103", env.Code)
	}
	if want := "An offset of 5 was requested, but there are only 4 rows."; env.Details != want {
		t.Errorf("details = %q, want %q", env.Details, want)
	}
}

// TestInvertedRangeHeaderIs416 checks a well-formed Range header whose upper
// bound is below its lower bound is the 416 range error, not silently ignored.
// A malformed header (TestMalformedRangeHeaderIgnored) still serves the full set.
func TestInvertedRangeHeaderIs416(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?order=id", map[string]string{
		"Range": "5-2",
	})
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", resp.StatusCode)
	}
	var env struct{ Code, Details string }
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "PGRST103" {
		t.Errorf("code = %q, want PGRST103", env.Code)
	}
	if want := "The lower boundary must be lower than or equal to the upper boundary in the Range header."; env.Details != want {
		t.Errorf("details = %q, want %q", env.Details, want)
	}
}

// TestMalformedRangeHeaderIgnored checks a non-numeric Range header is dropped
// rather than answered with 416: PostgREST serves the full result.
func TestMalformedRangeHeaderIgnored(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?order=id", map[string]string{
		"Range": "abc-def",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
