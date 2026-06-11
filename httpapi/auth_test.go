package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tamnd/dbrest/auth"
	"github.com/tamnd/dbrest/httpapi"
)

var authSecret = []byte("an-integration-test-secret-32bytes!")

// authServer is newServer with a JWT verifier attached.
func authServer(t *testing.T, cfg auth.Config) *httpapi.Server {
	t.Helper()
	if cfg.Secret == nil {
		cfg.Secret = authSecret
	}
	if cfg.AnonRole == "" {
		cfg.AnonRole = "anon"
	}
	v, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	srv := newServer(t)
	srv.SetVerifier(v)
	return srv
}

// mintToken signs an HS256 token with the integration secret.
func mintToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(authSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func TestNoTokenReadsAsAnon(t *testing.T) {
	srv := authServer(t, auth.Config{})
	resp := do(t, srv, http.MethodGet, "/films?select=id", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := len(decodeArray(t, resp)); got != 4 {
		t.Errorf("rows = %d, want 4", got)
	}
}

func TestValidTokenIsAccepted(t *testing.T) {
	srv := authServer(t, auth.Config{})
	tok := mintToken(t, jwt.MapClaims{
		"role": "web_user",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	resp := do(t, srv, http.MethodGet, "/films?select=id", map[string]string{
		"Authorization": "Bearer " + tok,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	srv := authServer(t, auth.Config{})
	tok := mintToken(t, jwt.MapClaims{
		"role": "web_user",
		"exp":  time.Now().Add(-time.Hour).Unix(),
	})
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{
		"Authorization": "Bearer " + tok,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["code"] != "PGRST303" {
		t.Errorf("code = %v, want PGRST303", body["code"])
	}
	want := `Bearer error="invalid_token", error_description="JWT expired"`
	if h := resp.Header.Get("WWW-Authenticate"); h != want {
		t.Errorf("WWW-Authenticate = %q, want %q", h, want)
	}
}

func TestGarbageTokenIsRejected(t *testing.T) {
	srv := authServer(t, auth.Config{})
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{
		"Authorization": "Bearer not-a-real-token",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["code"] != "PGRST301" {
		t.Errorf("code = %v, want PGRST301", body["code"])
	}
	if h := resp.Header.Get("WWW-Authenticate"); !strings.Contains(h, `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want the invalid_token challenge", h)
	}
}

func TestUnpermittedRoleIsForbidden(t *testing.T) {
	srv := authServer(t, auth.Config{PermittedRoles: []string{"web_user"}})
	tok := mintToken(t, jwt.MapClaims{
		"role": "admin",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{
		"Authorization": "Bearer " + tok,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestAuthRejectionShortCircuitsBeforeQuery(t *testing.T) {
	// A bad token on a write must be refused before the body is ever parsed.
	srv := authServer(t, auth.Config{})
	resp := send(t, srv, http.MethodPost, "/films", `not even json`, map[string]string{
		"Authorization": "Bearer garbage",
		"Content-Type":  "application/json",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSecretNeverEchoedInError(t *testing.T) {
	srv := authServer(t, auth.Config{})
	resp := do(t, srv, http.MethodGet, "/films", map[string]string{
		"Authorization": "Bearer bad.token.here",
	})
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	for _, v := range body {
		if s, ok := v.(string); ok && strings.Contains(s, string(authSecret)) {
			t.Fatalf("error body leaked the secret: %q", s)
		}
	}
}
