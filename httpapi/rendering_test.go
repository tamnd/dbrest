package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// readBody returns the response body as a string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestSelectOrderPreservedInJSON pins 02.19: object keys appear in projection
// order, not alphabetized. A select of title,id renders {"title":...,"id":...}.
func TestSelectOrderPreservedInJSON(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.1&select=title,id", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	titlePos := strings.Index(body, `"title"`)
	idPos := strings.Index(body, `"id"`)
	if titlePos < 0 || idPos < 0 {
		t.Fatalf("body missing expected keys: %s", body)
	}
	if titlePos > idPos {
		t.Errorf("keys out of select order, want title before id: %s", body)
	}
}

// TestNullsStrippedArrayOmitsNullKeys pins 02.13: the nulls=stripped parameter
// on the vendor array type drops null-valued keys from each object and the
// Content-Type echoes the parameter. Film 4 has a NULL year.
func TestNullsStrippedArrayOmitsNullKeys(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.4&select=id,title,year,rating", map[string]string{
		"Accept": "application/vnd.pgrst.array+json;nulls=stripped",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.pgrst.array+json; nulls=stripped; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := readBody(t, resp)
	if strings.Contains(body, `"year"`) {
		t.Errorf("null year key should be stripped: %s", body)
	}
	if !strings.Contains(body, `"title"`) || !strings.Contains(body, `"rating"`) {
		t.Errorf("non-null keys should remain: %s", body)
	}
}

// TestNullsStrippedObjectOmitsNullKeys pins the same parameter on the object
// vendor type for a singular request.
func TestNullsStrippedObjectOmitsNullKeys(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.4&select=id,title,year", map[string]string{
		"Accept": "application/vnd.pgrst.object+json;nulls=stripped",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.pgrst.object+json; nulls=stripped; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := readBody(t, resp)
	if strings.Contains(body, `"year"`) {
		t.Errorf("null year key should be stripped: %s", body)
	}
	if strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Errorf("object request should not be an array: %s", body)
	}
}

// TestNullsStrippedIgnoredOnPlainJSON pins that the parameter is vendor-only:
// plain application/json keeps null keys, matching PostgREST.
func TestNullsStrippedIgnoredOnPlainJSON(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.4&select=id,title,year", map[string]string{
		"Accept": "application/json;nulls=stripped",
	})
	body := readBody(t, resp)
	if !strings.Contains(body, `"year"`) {
		t.Errorf("plain json should keep the null key: %s", body)
	}
}

// TestCSVQuotesAndNoTrailingBlankLine pins 02.20: a field with a comma and a
// double quote is RFC 4180 quoted the way PostgREST quotes CSV (comma forces
// quoting, inner quotes are doubled), records are \n-terminated, and there is no
// extra trailing blank line after the last record.
func TestCSVQuotesAndNoTrailingBlankLine(t *testing.T) {
	srv := newServer(t)
	send(t, srv, http.MethodPost, "/films", `{"id":91,"title":"A, B \"q\""}`, map[string]string{
		"Prefer": "return=minimal",
	})
	resp := do(t, srv, http.MethodGet, "/films?id=eq.91&select=id,title", map[string]string{
		"Accept": "text/csv",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	want := "id,title\n91,\"A, B \"\"q\"\"\"\n"
	if body != want {
		t.Errorf("CSV body = %q, want %q", body, want)
	}
}

// TestCSVEmptyResultKeepsHeader pins the empty-result CSV shape dbrest produces:
// the column-name header line plus a newline, with no data rows. The PostgREST
// empty-result shape itself is verified separately against a live server (02.20).
func TestCSVEmptyResultKeepsHeader(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.9999&select=id,title", map[string]string{
		"Accept": "text/csv",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body := readBody(t, resp); body != "id,title\n" {
		t.Errorf("empty CSV body = %q, want the header line only", body)
	}
}

// TestSelectOrderReversed pins the inverse projection to prove the order tracks
// the select, not a fixed column order: id,title renders {"id":...,"title":...}.
func TestSelectOrderReversed(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?id=eq.1&select=year,rating,title", nil)
	body := readBody(t, resp)
	yearPos := strings.Index(body, `"year"`)
	ratingPos := strings.Index(body, `"rating"`)
	titlePos := strings.Index(body, `"title"`)
	if !(yearPos < ratingPos && ratingPos < titlePos) {
		t.Errorf("keys not in select order year,rating,title: %s", body)
	}
}
