package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The write-response Content-Range is shaped by method, not by the return mode
// (02.8): POST and DELETE report the total-only "*/*" form ("*/N" with
// count=exact), PATCH reports the affected-row range, and PUT reports none.

func TestPostContentRangeIsTotalOnly(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":30,"title":"X"}`, map[string]string{
		"Prefer": "return=minimal",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	// Present even with return=minimal and no count.
	if cr := resp.Header.Get("Content-Range"); cr != "*/*" {
		t.Errorf("Content-Range = %q, want */*", cr)
	}
}

func TestPostContentRangeWithCount(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":31,"title":"X"}`, map[string]string{
		"Prefer": "return=minimal, count=exact",
	})
	if cr := resp.Header.Get("Content-Range"); cr != "*/1" {
		t.Errorf("Content-Range = %q, want */1", cr)
	}
}

func TestDeleteContentRangeIsTotalOnly(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodDelete, "/films?id=eq.3", "", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "*/*" {
		t.Errorf("Content-Range = %q, want */*", cr)
	}
}

func TestDeleteContentRangeWithCount(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodDelete, "/films?id=eq.3", "", map[string]string{
		"Prefer": "count=exact",
	})
	if cr := resp.Header.Get("Content-Range"); cr != "*/1" {
		t.Errorf("Content-Range = %q, want */1", cr)
	}
}

func TestPatchContentRangeIsRowSpan(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.2", `{"rating":"PG"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if cr := resp.Header.Get("Content-Range"); cr != "0-0/*" {
		t.Errorf("Content-Range = %q, want 0-0/*", cr)
	}
}

func TestPatchContentRangeWithCount(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?year=gte.1980", `{"rating":"PG"}`, map[string]string{
		"Prefer": "return=representation, count=exact",
	})
	if cr := resp.Header.Get("Content-Range"); cr != "0-1/2" {
		t.Errorf("Content-Range = %q, want 0-1/2", cr)
	}
}

// A PATCH on the minimal path still carries the row span, since Content-Range
// does not depend on the return mode.
func TestPatchMinimalStillCarriesContentRange(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.2", `{"rating":"PG"}`, map[string]string{
		"Prefer": "return=minimal",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "0-0/*" {
		t.Errorf("Content-Range = %q, want 0-0/*", cr)
	}
}

// A PATCH that matches no row reports the empty-span "*/*" form (and "*/0" with
// count=exact), not a negative range.
func TestPatchNoMatchContentRange(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPatch, "/films?id=eq.999", `{"rating":"PG"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if cr := resp.Header.Get("Content-Range"); cr != "*/*" {
		t.Errorf("Content-Range = %q, want */*", cr)
	}
	withCount := send(t, srv, http.MethodPatch, "/films?id=eq.999", `{"rating":"PG"}`, map[string]string{
		"Prefer": "return=representation, count=exact",
	})
	if cr := withCount.Header.Get("Content-Range"); cr != "*/0" {
		t.Errorf("Content-Range = %q, want */0", cr)
	}
}

// A PUT carries no Content-Range in any return mode (02.8, 02.9).
func TestPutHasNoContentRange(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPut, "/films?id=eq.2", `{"id":2,"title":"Blade Runner"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		t.Errorf("Content-Range = %q, want none on PUT", cr)
	}
}

// A PUT with no representation answers 204 with an empty body and no Location or
// Content-Range, the same for return=minimal, headers-only, and no preference
// (02.9).
func TestPutWithoutRepresentationIs204(t *testing.T) {
	cases := []struct {
		name   string
		prefer string
	}{
		{"no-preference", ""},
		{"minimal", "return=minimal"},
		{"headers-only", "return=headers-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newServer(t)
			headers := map[string]string{}
			if c.prefer != "" {
				headers["Prefer"] = c.prefer
			}
			resp := send(t, srv, http.MethodPut, "/films?id=eq.2", `{"id":2,"title":"Replaced"}`, headers)
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if len(body) != 0 {
				t.Errorf("body = %q, want empty", body)
			}
			if loc := resp.Header.Get("Location"); loc != "" {
				t.Errorf("Location = %q, want none on PUT", loc)
			}
			if cr := resp.Header.Get("Content-Range"); cr != "" {
				t.Errorf("Content-Range = %q, want none on PUT", cr)
			}
			// The replacement persisted.
			after := do(t, srv, http.MethodGet, "/films?id=eq.2&select=title", nil)
			rows := decodeArray(t, after)
			if len(rows) != 1 || rows[0]["title"] != "Replaced" {
				t.Errorf("after PUT = %v, want title Replaced", rows)
			}
		})
	}
}

// An ignore-duplicates upsert that hits only existing rows inserts nothing, yet
// PostgREST still reports 201 (only merge-duplicates with zero inserted is 200).
func TestPostUpsertIgnoreDuplicatesExistingIs201(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":1,"title":"Ignored"}`, map[string]string{
		"Prefer": "return=minimal, resolution=ignore-duplicates",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	// The existing row was left untouched.
	after := do(t, srv, http.MethodGet, "/films?id=eq.1&select=title", nil)
	rows := decodeArray(t, after)
	if len(rows) != 1 || rows[0]["title"] != "Metropolis" {
		t.Errorf("ignore-duplicates altered the row: %v", rows)
	}
}

// A merge-duplicates upsert whose batch mixes a new key with an existing one
// inserted at least one row, so the status is 201, not 200.
func TestPostUpsertMergeMixedBatchIs201(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films",
		`[{"id":1,"title":"Metropolis v2"},{"id":70,"title":"Fresh"}]`, map[string]string{
			"Prefer": "return=representation, resolution=merge-duplicates",
		})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for a mixed batch", resp.StatusCode)
	}
}

// A return=headers-only POST insert of a single row keeps its Location header,
// the one write that still carries one.
func TestPostHeadersOnlyKeepsLocation(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", `{"id":80,"title":"Located"}`, map[string]string{
		"Prefer": "return=headers-only",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "id=eq.80") {
		t.Errorf("Location = %q, want it to address id=eq.80", loc)
	}
}
