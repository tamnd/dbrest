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
