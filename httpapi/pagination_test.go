package httpapi_test

import (
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
}
