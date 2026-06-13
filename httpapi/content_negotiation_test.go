package httpapi_test

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeEnvelope(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env
}

func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestGetCSV(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=id,title&order=id", map[string]string{
		"Accept": "text/csv",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	recs, err := csv.NewReader(strings.NewReader(bodyString(t, resp))).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(recs) != 5 { // header + 4 rows
		t.Fatalf("got %d csv records, want 5", len(recs))
	}
	if recs[0][0] != "id" || recs[0][1] != "title" {
		t.Errorf("header = %v, want [id title]", recs[0])
	}
	if recs[1][1] != "Metropolis" {
		t.Errorf("first row title = %q", recs[1][1])
	}
}

func TestGetCSVNullIsEmptyField(t *testing.T) {
	srv := newServer(t)
	// Film 4 (Untitled) has a NULL year; it must render as an empty CSV cell.
	resp := do(t, srv, http.MethodGet, "/films?select=title,year&id=eq.4", map[string]string{
		"Accept": "text/csv",
	})
	recs, err := csv.NewReader(strings.NewReader(bodyString(t, resp))).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(recs) != 2 || recs[1][0] != "Untitled" || recs[1][1] != "" {
		t.Fatalf("rows = %v, want Untitled with empty year", recs)
	}
}

func TestGetTextPlainSingleColumn(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title&id=eq.2", map[string]string{
		"Accept": "text/plain",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := bodyString(t, resp); got != "Blade Runner" {
		t.Errorf("body = %q, want Blade Runner", got)
	}
}

func TestGetOctetStreamSingleColumn(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=title&id=eq.3", map[string]string{
		"Accept": "application/octet-stream",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := bodyString(t, resp); got != "Arrival" {
		t.Errorf("body = %q, want Arrival", got)
	}
}

func TestScalarMultiColumnIsError(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films?select=id,title&id=eq.2", map[string]string{
		"Accept": "text/plain",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for multi-column scalar", resp.StatusCode)
	}
}

func TestGetNotAcceptable(t *testing.T) {
	srv := newServer(t)
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{
		"Accept": "application/xml",
	})
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env["code"] != "PGRST107" {
		t.Errorf("code = %v, want PGRST107", env["code"])
	}
}

func TestPostCSVInsert(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", "id,title,year\n30,CsvFilm,1999\n", map[string]string{
		"Content-Type": "text/csv",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	// The row landed and reads back.
	after := do(t, srv, http.MethodGet, "/films?id=eq.30", nil)
	rows := decodeArray(t, after)
	if len(rows) != 1 || rows[0]["title"] != "CsvFilm" {
		t.Fatalf("inserted row = %v", rows)
	}
}

func TestPostCSVBulkInsert(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films",
		"id,title\n40,A\n41,B\n42,C\n", map[string]string{
			"Content-Type": "text/csv",
			"Prefer":       "return=representation",
		})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if len(decodeArray(t, resp)) != 3 {
		t.Error("want 3 inserted rows")
	}
}

func TestPostFormInsert(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", "id=31&title=FormFilm", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	after := do(t, srv, http.MethodGet, "/films?id=eq.31", nil)
	rows := decodeArray(t, after)
	if len(rows) != 1 || rows[0]["title"] != "FormFilm" {
		t.Fatalf("inserted row = %v", rows)
	}
}

func TestPostUnsupportedMediaType(t *testing.T) {
	srv := newServer(t)
	resp := send(t, srv, http.MethodPost, "/films", "<film/>", map[string]string{
		"Content-Type": "application/xml",
	})
	// Live v14 answers 400 PGRST102 for an unparseable request Content-Type;
	// the docs' PGRST107/415 row is stale (see compat/errors_v14_test.go).
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeEnvelope(t, resp)
	if env["code"] != "PGRST102" {
		t.Errorf("code = %v, want PGRST102", env["code"])
	}
}

func TestPostRepresentationAsCSV(t *testing.T) {
	srv := newServer(t)
	// JSON body in, CSV representation out: the request and response formats are
	// negotiated independently.
	resp := send(t, srv, http.MethodPost, "/films", `{"id":50,"title":"Mixed","year":2003}`, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "text/csv",
		"Prefer":       "return=representation",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	recs, err := csv.NewReader(strings.NewReader(bodyString(t, resp))).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("csv representation = %v (err %v)", recs, err)
	}
}

func BenchmarkGetCSV(b *testing.B) {
	srv := newServer(b)
	req := httptest.NewRequest(http.MethodGet, "/films?select=id,title,year&order=id", nil)
	req.Header.Set("Accept", "text/csv")
	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}
