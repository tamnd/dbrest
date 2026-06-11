// Auth wire-compat cases against PostgREST v14: the PGRST301/302/303 code
// assignments, the WWW-Authenticate challenges, and the claim validation
// behavior. Each case is sent to both servers and the status, the JSON error
// envelope, and the WWW-Authenticate header must agree byte for byte.
//
// The servers come from the same compose stacks as compat_test.go and share
// the jwt-secret "reallyreallyreallyreallyverysafe"; tokens are minted here so
// the time claims are relative to the test run.
package compat

import (
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// compatSecret is the jwt-secret both compose stacks are configured with.
var compatSecret = []byte("reallyreallyreallyreallyverysafe")

// mintHS signs an HS256 token with the shared compat secret.
func mintHS(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(compatSecret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// authCase is one auth wire comparison: the request is sent to both servers
// and status, JSON body, and WWW-Authenticate must match across them.
type authCase struct {
	name   string
	method string
	path   string
	token  string // Authorization: Bearer <token> when non-empty
	header map[string]string

	wantStatus int // when > 0 both servers must return exactly this
}

// runAuthCases drives the cross-server comparison for a case list.
func runAuthCases(t *testing.T, cases []authCase) {
	t.Helper()
	pgrest, dbrest := urls(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			headers := map[string]string{}
			for k, v := range c.header {
				headers[k] = v
			}
			if c.token != "" {
				headers["Authorization"] = "Bearer " + c.token
			}
			cc := compatCase{method: c.method, path: c.path, headers: headers}
			pgResp := doRequest(t, pgrest, cc)
			dbResp := doRequest(t, dbrest, cc)

			if pgResp.status != dbResp.status {
				t.Errorf("status: postgrest=%d dbrest=%d", pgResp.status, dbResp.status)
			}
			if c.wantStatus != 0 && dbResp.status != c.wantStatus {
				t.Errorf("dbrest status = %d, want %d", dbResp.status, c.wantStatus)
			}
			pgWWW := pgResp.header.Get("WWW-Authenticate")
			dbWWW := dbResp.header.Get("WWW-Authenticate")
			if pgWWW != dbWWW {
				t.Errorf("WWW-Authenticate: postgrest=%q dbrest=%q", pgWWW, dbWWW)
			}
			if dbResp.status >= 400 {
				compareJSON(t, pgResp, dbResp)
			}
		})
	}
}

// The group-3 code assignments and the WWW-Authenticate surface (item 03.1):
// PGRST301 for decode failures with per-cause messages, PGRST303 for claim
// validation failures, the invalid_token challenge on both.
func TestV14AuthErrorSurface(t *testing.T) {
	expired := mintHS(t, jwt.MapClaims{
		"role": "web_user",
		"exp":  time.Now().Add(-time.Hour).Unix(),
	})
	notYet := mintHS(t, jwt.MapClaims{
		"role": "web_user",
		"nbf":  time.Now().Add(time.Hour).Unix(),
	})
	good := mintHS(t, jwt.MapClaims{
		"role": "web_user",
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	badSig := good[:len(good)-2] + "qq"

	runAuthCases(t, []authCase{
		{name: "expired token is 401 PGRST303", method: http.MethodGet, path: "/todos",
			token: expired, wantStatus: 401},
		{name: "not-yet-valid token is 401 PGRST303", method: http.MethodGet, path: "/todos",
			token: notYet, wantStatus: 401},
		{name: "one-part token reports the part count", method: http.MethodGet, path: "/todos",
			token: "garbage", wantStatus: 401},
		{name: "two-part token reports the part count", method: http.MethodGet, path: "/todos",
			token: "two.parts", wantStatus: 401},
		{name: "bad signature is 401 PGRST301", method: http.MethodGet, path: "/todos",
			token: badSig, wantStatus: 401},
		{name: "valid token reads fine", method: http.MethodGet, path: "/todos",
			token: good, wantStatus: 200},
	})
}

// The claim validation surface (item 03.5): iat is validated with skew, type
// errors carry their own PGRST303 messages, and a token without aud (or with a
// foreign aud, since neither stack configures jwt-aud) is accepted.
func TestV14ClaimValidation(t *testing.T) {
	iatFuture := mintHS(t, jwt.MapClaims{
		"role": "web_user",
		"iat":  time.Now().Add(time.Hour).Unix(),
	})
	expString := mintHS(t, jwt.MapClaims{"role": "web_user", "exp": "soon"})
	iatString := mintHS(t, jwt.MapClaims{"role": "web_user", "iat": "x"})
	foreignAud := mintHS(t, jwt.MapClaims{"role": "web_user", "aud": "other"})
	emptyAud := mintHS(t, jwt.MapClaims{"role": "web_user", "aud": []string{}})

	runAuthCases(t, []authCase{
		{name: "future iat is 401 PGRST303", method: http.MethodGet, path: "/todos",
			token: iatFuture, wantStatus: 401},
		{name: "non-number exp is a type error", method: http.MethodGet, path: "/todos",
			token: expString, wantStatus: 401},
		{name: "non-number iat is a type error", method: http.MethodGet, path: "/todos",
			token: iatString, wantStatus: 401},
		{name: "foreign aud passes with no jwt-aud configured", method: http.MethodGet, path: "/todos",
			token: foreignAud, wantStatus: 200},
		{name: "empty aud array passes", method: http.MethodGet, path: "/todos",
			token: emptyAud, wantStatus: 200},
	})
}
