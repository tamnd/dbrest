package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fixedClock returns a clock pinned to a fixed instant for deterministic tests.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// the test clock and a secret long enough to pass the startup check.
var (
	clockNow = time.Unix(1_749_200_000, 0)
	hmacKey  = []byte("a-test-secret-that-is-long-enough!!")
	anonRole = "anon"
	testAud  = "dbrest"
)

// signHS mints an HS256 token with the given claims using the test secret.
func signHS(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(hmacKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// hmacVerifier builds a verifier over the test HMAC secret and clock.
func hmacVerifier(t *testing.T, cfg Config) *Verifier {
	t.Helper()
	if cfg.Secret == nil {
		cfg.Secret = hmacKey
	}
	if cfg.AnonRole == "" {
		cfg.AnonRole = anonRole
	}
	v, err := NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.now = fixedClock(clockNow)
	return v
}

func TestNoBearerRunsAnon(t *testing.T) {
	v := hmacVerifier(t, Config{})
	res, err := v.Authenticate("")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.Role != anonRole || !res.Anonymous {
		t.Fatalf("no token = %+v, want anon", res)
	}
}

func TestValidTokenResolvesRole(t *testing.T) {
	v := hmacVerifier(t, Config{})
	tok := signHS(t, jwt.MapClaims{
		"role": "web_user",
		"exp":  clockNow.Add(time.Hour).Unix(),
	})
	res, err := v.Authenticate("Bearer " + tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.Role != "web_user" || res.Anonymous {
		t.Fatalf("res = %+v, want web_user", res)
	}
	if res.Claims["role"] != "web_user" {
		t.Errorf("claims not published: %+v", res.Claims)
	}
}

func TestBearerSchemeCaseInsensitive(t *testing.T) {
	v := hmacVerifier(t, Config{})
	tok := signHS(t, jwt.MapClaims{"role": "web_user"})
	res, err := v.Authenticate("bEaReR " + tok)
	if err != nil || res.Role != "web_user" {
		t.Fatalf("lowercase bearer: %+v, %v", res, err)
	}
}

func TestTokenWithNoRoleFallsBackToAnon(t *testing.T) {
	v := hmacVerifier(t, Config{})
	tok := signHS(t, jwt.MapClaims{"sub": "123"})
	res, err := v.Authenticate("Bearer " + tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.Role != anonRole {
		t.Fatalf("role = %q, want anon fallback", res.Role)
	}
	if res.Anonymous {
		t.Error("a verified token is not anonymous even when it resolves to anon")
	}
}

func TestExpiredTokenIs303(t *testing.T) {
	v := hmacVerifier(t, Config{})
	tok := signHS(t, jwt.MapClaims{
		"role": "web_user",
		"exp":  clockNow.Add(-time.Hour).Unix(),
	})
	_, err := v.Authenticate("Bearer " + tok)
	if err == nil || err.Code != "PGRST303" || err.Message != "JWT expired" {
		t.Fatalf("want PGRST303 JWT expired, got %v", err)
	}
	if err.HTTPStatus != 401 {
		t.Errorf("status = %d, want 401", err.HTTPStatus)
	}
	want := `Bearer error="invalid_token", error_description="JWT expired"`
	if err.WWWAuthenticate != want {
		t.Errorf("WWW-Authenticate = %q, want %q", err.WWWAuthenticate, want)
	}
}

func TestExpiryWithinSkewStillValid(t *testing.T) {
	v := hmacVerifier(t, Config{})
	// expired 10s ago, inside the 30s skew window.
	tok := signHS(t, jwt.MapClaims{
		"role": "web_user",
		"exp":  clockNow.Add(-10 * time.Second).Unix(),
	})
	if _, err := v.Authenticate("Bearer " + tok); err != nil {
		t.Fatalf("a token within skew must verify: %v", err)
	}
}

func TestNotBeforeIs303(t *testing.T) {
	v := hmacVerifier(t, Config{})
	tok := signHS(t, jwt.MapClaims{
		"role": "web_user",
		"nbf":  clockNow.Add(time.Hour).Unix(),
	})
	_, err := v.Authenticate("Bearer " + tok)
	if err == nil || err.Code != "PGRST303" || err.Message != "JWT not yet valid" {
		t.Fatalf("want PGRST303 JWT not yet valid, got %v", err)
	}
}

func TestBadSignatureIs301(t *testing.T) {
	v := hmacVerifier(t, Config{})
	tok := signHS(t, jwt.MapClaims{"role": "web_user"})
	// flip the last character of the signature.
	bad := tok[:len(tok)-1] + flip(tok[len(tok)-1])
	_, err := v.Authenticate("Bearer " + bad)
	if err == nil || err.Code != "PGRST301" || err.Message != "No suitable key or wrong key type" {
		t.Fatalf("want PGRST301 No suitable key or wrong key type, got %v", err)
	}
	if err.Details == nil || *err.Details != "None of the keys was able to decode the JWT" {
		t.Errorf("details = %v, want the none-of-the-keys detail", err.Details)
	}
}

func TestMalformedTokenIs301(t *testing.T) {
	v := hmacVerifier(t, Config{})
	_, err := v.Authenticate("Bearer not.a.jwt")
	if err == nil || err.Code != "PGRST301" || err.Message != "JWT cryptographic operation failed" {
		t.Fatalf("want PGRST301 JWT cryptographic operation failed, got %v", err)
	}
}

func TestWrongPartCountMessage(t *testing.T) {
	v := hmacVerifier(t, Config{})
	cases := []struct {
		token string
		want  string
	}{
		{"justonepart", "Expected 3 parts in JWT; got 1"},
		{"two.parts", "Expected 3 parts in JWT; got 2"},
		{"a.b.c.d", "Expected 3 parts in JWT; got 4"},
	}
	for _, c := range cases {
		_, err := v.Authenticate("Bearer " + c.token)
		if err == nil || err.Code != "PGRST301" || err.Message != c.want {
			t.Errorf("token %q: want PGRST301 %q, got %v", c.token, c.want, err)
		}
	}
}

func TestNoneAlgorithmRejected(t *testing.T) {
	v := hmacVerifier(t, Config{})
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"role": "admin"})
	s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	_, aerr := v.Authenticate("Bearer " + s)
	if aerr == nil || aerr.Code != "PGRST301" || aerr.Message != "Wrong or unsupported encoding algorithm" {
		t.Fatalf("the none alg must be rejected with PGRST301, got %v", aerr)
	}
	if aerr.Details == nil || *aerr.Details != "JWT is unsecured but expected 'alg' was not 'none'" {
		t.Errorf("details = %v, want the unsecured-token detail", aerr.Details)
	}
}

func TestNoneInAllowedAlgsRefusedAtStartup(t *testing.T) {
	_, err := NewVerifier(Config{Secret: hmacKey, AllowedAlgs: []string{"HS256", "none"}})
	if err == nil {
		t.Fatal("a config that allows none must fail to build")
	}
}

func TestShortSecretRefusedAtStartup(t *testing.T) {
	_, err := NewVerifier(Config{Secret: []byte("too-short")})
	if err == nil {
		t.Fatal("a sub-32-byte secret must be refused at startup")
	}
}

func TestAudienceEnforced(t *testing.T) {
	v := hmacVerifier(t, Config{Audience: testAud})
	good := signHS(t, jwt.MapClaims{"role": "web_user", "aud": testAud})
	if _, err := v.Authenticate("Bearer " + good); err != nil {
		t.Fatalf("matching aud must verify: %v", err)
	}
	bad := signHS(t, jwt.MapClaims{"role": "web_user", "aud": "other"})
	if _, err := v.Authenticate("Bearer " + bad); err == nil || err.Code != "PGRST303" || err.Message != "JWT not in audience" {
		t.Fatalf("wrong aud must be PGRST303 JWT not in audience, got %v", err)
	}
}

func TestNestedRoleClaim(t *testing.T) {
	v := hmacVerifier(t, Config{RoleClaimKey: ".app_metadata.role"})
	tok := signHS(t, jwt.MapClaims{
		"app_metadata": map[string]any{"role": "web_user"},
	})
	res, err := v.Authenticate("Bearer " + tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.Role != "web_user" {
		t.Fatalf("nested role = %q, want web_user", res.Role)
	}
}

func TestPermittedRoleAllowed(t *testing.T) {
	v := hmacVerifier(t, Config{PermittedRoles: []string{"web_user"}})
	tok := signHS(t, jwt.MapClaims{"role": "web_user"})
	if _, err := v.Authenticate("Bearer " + tok); err != nil {
		t.Fatalf("a permitted role must pass: %v", err)
	}
}

func TestUnpermittedRoleIs403(t *testing.T) {
	v := hmacVerifier(t, Config{PermittedRoles: []string{"web_user"}})
	tok := signHS(t, jwt.MapClaims{"role": "admin"})
	_, err := v.Authenticate("Bearer " + tok)
	if err == nil || err.HTTPStatus != 403 {
		t.Fatalf("an unpermitted role must be 403, got %v", err)
	}
	if err.Code != "42501" {
		t.Errorf("code = %q, want 42501", err.Code)
	}
}

func TestAnonDisabledWithoutToken(t *testing.T) {
	v, err := NewVerifier(Config{Secret: hmacKey}) // no anon role
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	_, aerr := v.Authenticate("")
	if aerr == nil || aerr.HTTPStatus != 401 {
		t.Fatalf("no anon role + no token must be 401, got %v", aerr)
	}
}

func TestNoKeysDisablesVerification(t *testing.T) {
	v, err := NewVerifier(Config{AnonRole: anonRole})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	// A token is presented but no key is configured: it runs as anon.
	res, aerr := v.Authenticate("Bearer anything.at.all")
	if aerr != nil {
		t.Fatalf("Authenticate: %v", aerr)
	}
	if res.Role != anonRole {
		t.Fatalf("role = %q, want anon when verification is off", res.Role)
	}
}

func TestRSAVerification(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	v := rsaVerifier(t, &key.PublicKey)

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"role": "web_user"})
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	res, aerr := v.Authenticate("Bearer " + signed)
	if aerr != nil {
		t.Fatalf("Authenticate: %v", aerr)
	}
	if res.Role != "web_user" {
		t.Fatalf("role = %q, want web_user", res.Role)
	}
}

func TestAlgConfusionRejected(t *testing.T) {
	// An RSA verifier must not accept an HS256 token signed with the public key
	// bytes used as an HMAC secret.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	v := rsaVerifier(t, &key.PublicKey)

	pubPEM := publicKeyPEM(t, &key.PublicKey)
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"role": "admin"})
	signed, err := forged.SignedString([]byte(pubPEM))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, aerr := v.Authenticate("Bearer " + signed)
	if aerr == nil {
		t.Fatal("an HS256 token must not verify against an RSA-only verifier")
	}
	if aerr.Code != "PGRST301" || aerr.Message != "No suitable key or wrong key type" {
		t.Errorf("want PGRST301 No suitable key or wrong key type, got %v", aerr)
	}
}

func TestECDSAVerification(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	v, err := NewVerifier(Config{PublicKeyPEM: pemText, AnonRole: anonRole})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.now = fixedClock(clockNow)

	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"role": "web_user"})
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	res, aerr := v.Authenticate("Bearer " + signed)
	if aerr != nil {
		t.Fatalf("Authenticate: %v", aerr)
	}
	if res.Role != "web_user" {
		t.Fatalf("role = %q, want web_user", res.Role)
	}
}

// rsaVerifier builds a verifier over an RSA public key with the test clock.
func rsaVerifier(t *testing.T, pub *rsa.PublicKey) *Verifier {
	t.Helper()
	v, err := NewVerifier(Config{PublicKeyPEM: publicKeyPEM(t, pub), AnonRole: anonRole})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.now = fixedClock(clockNow)
	return v
}

// publicKeyPEM encodes a public key as PKIX PEM text.
func publicKeyPEM(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// flip returns a byte distinct from b, to corrupt a signature character.
func flip(b byte) string {
	if b == 'A' {
		return "B"
	}
	return "A"
}
