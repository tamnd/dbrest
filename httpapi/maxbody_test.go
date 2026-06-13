package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUnlimitedBodyByDefault checks dbrest imposes no body cap out of the box,
// matching PostgREST: a bulk insert far past the old 16 MiB limit is accepted.
func TestUnlimitedBodyByDefault(t *testing.T) {
	srv := newServer(t)

	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 20000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"title":"`)
		sb.WriteString(strings.Repeat("x", 64))
		sb.WriteString(`"}`)
	}
	sb.WriteByte(']')

	req := httptest.NewRequest(http.MethodPost, "/films", strings.NewReader(sb.String()))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestMaxRequestBodyRejectsOversizeWith413 checks a configured cap answers an
// oversize body with 413 PGRSTX13 and the byte bound, not a parse error.
func TestMaxRequestBodyRejectsOversizeWith413(t *testing.T) {
	srv := newServer(t)
	srv.SetMaxRequestBody(32)

	big := `[{"title":"` + strings.Repeat("x", 200) + `"}]`
	req := httptest.NewRequest(http.MethodPost, "/films", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
	var env struct{ Code, Message string }
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Code != "PGRSTX13" {
		t.Errorf("code = %q, want PGRSTX13", env.Code)
	}
	if !strings.Contains(env.Message, "32") {
		t.Errorf("message %q should name the 32 byte bound", env.Message)
	}
}

// TestMaxRequestBodyAllowsUnderCap checks a body within the configured cap is
// processed normally.
func TestMaxRequestBodyAllowsUnderCap(t *testing.T) {
	srv := newServer(t)
	srv.SetMaxRequestBody(1 << 20)

	req := httptest.NewRequest(http.MethodPost, "/films", strings.NewReader(`{"title":"Solaris"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}
