package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// octJWK renders an HMAC secret as a symmetric JWK with an optional kid.
func octJWK(secret []byte, kid string) string {
	w := map[string]string{
		"kty": "oct",
		"k":   base64.RawURLEncoding.EncodeToString(secret),
	}
	if kid != "" {
		w["kid"] = kid
	}
	b, _ := json.Marshal(w)
	return string(b)
}

// signWithKid mints an HS256 token carrying a kid header.
func signWithKid(t *testing.T, secret []byte, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// jwt-secret can hold a single JWK: the symmetric key inside verifies HS256.
func TestSecretAsSingleJWK(t *testing.T) {
	secret := []byte("jwk-borne-secret-thats-not-the-text!")
	v := hmacVerifier(t, Config{Secret: []byte(octJWK(secret, ""))})
	tok := signWithKid(t, secret, "", jwt.MapClaims{"role": "web_user"})
	res, err := v.Authenticate("Bearer " + tok)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.Role != "web_user" {
		t.Fatalf("role = %q", res.Role)
	}
}

// jwt-secret can hold a JWK Set; the token's kid picks its key, a kid-less
// token tries each key, and an unknown kid is a PGRST301.
func TestSecretAsJWKSetWithKid(t *testing.T) {
	k1 := []byte("first-shared-secret-32-bytes-long!!!")
	k2 := []byte("second-shared-secret-32-bytes-long!!")
	set := fmt.Sprintf(`{"keys":[%s,%s]}`, octJWK(k1, "one"), octJWK(k2, "two"))
	v := hmacVerifier(t, Config{Secret: []byte(set)})

	// kid selects the second key.
	tok := signWithKid(t, k2, "two", jwt.MapClaims{"role": "web_user"})
	res, err := v.Authenticate("Bearer " + tok)
	if err != nil || res.Role != "web_user" {
		t.Fatalf("kid-selected key: %+v, %v", res, err)
	}

	// a kid signed with the wrong key fails the signature check.
	tok = signWithKid(t, k2, "one", jwt.MapClaims{"role": "web_user"})
	if _, err := v.Authenticate("Bearer " + tok); err == nil || err.Code != "PGRST301" {
		t.Fatalf("wrong key for kid must be PGRST301, got %v", err)
	}

	// an unknown kid leaves no candidate keys.
	tok = signWithKid(t, k1, "ghost", jwt.MapClaims{"role": "web_user"})
	_, aerr := v.Authenticate("Bearer " + tok)
	if aerr == nil || aerr.Code != "PGRST301" || aerr.Message != "No suitable key or wrong key type" {
		t.Fatalf("unknown kid must be a no-suitable-key PGRST301, got %v", aerr)
	}

	// a kid-less token tries every key and verifies with the second.
	tok = signWithKid(t, k2, "", jwt.MapClaims{"role": "web_user"})
	res, err = v.Authenticate("Bearer " + tok)
	if err != nil || res.Role != "web_user" {
		t.Fatalf("kid-less try-all: %+v, %v", res, err)
	}
}

// jwt-secret can hold an RSA JWK and an EC JWK; RS256 and ES256 tokens verify
// against them.
func TestSecretAsAsymmetricJWK(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	rsaJWK, _ := json.Marshal(map[string]string{
		"kty": "RSA",
		"n":   base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes()),
	})
	ecJWK, _ := json.Marshal(map[string]string{
		"kty": "EC",
		"crv": "P-256",
		"x":   base64.RawURLEncoding.EncodeToString(ecKey.X.Bytes()),
		"y":   base64.RawURLEncoding.EncodeToString(ecKey.Y.Bytes()),
	})
	set := fmt.Sprintf(`{"keys":[%s,%s]}`, rsaJWK, ecJWK)
	v := hmacVerifier(t, Config{Secret: []byte(set)})

	rsTok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"role": "web_user"})
	signed, err := rsTok.SignedString(rsaKey)
	if err != nil {
		t.Fatalf("sign rs: %v", err)
	}
	if res, aerr := v.Authenticate("Bearer " + signed); aerr != nil || res.Role != "web_user" {
		t.Fatalf("RS256 against RSA JWK: %+v, %v", res, aerr)
	}

	esTok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"role": "web_user"})
	signed, err = esTok.SignedString(ecKey)
	if err != nil {
		t.Fatalf("sign es: %v", err)
	}
	if res, aerr := v.Authenticate("Bearer " + signed); aerr != nil || res.Role != "web_user" {
		t.Fatalf("ES256 against EC JWK: %+v, %v", res, aerr)
	}
}

// A JSON value that is neither a JWK nor a JWK Set falls through to the text
// secret interpretation, the same reading PostgREST applies.
func TestMalformedJWKFallsBackToText(t *testing.T) {
	secret := []byte(`{"not_a_jwk": "but long enough to be a passphrase"}`)
	v := hmacVerifier(t, Config{Secret: secret})
	tok := signWithKid(t, secret, "", jwt.MapClaims{"role": "web_user"})
	res, err := v.Authenticate("Bearer " + tok)
	if err != nil || res.Role != "web_user" {
		t.Fatalf("text fallback: %+v, %v", res, err)
	}
}

// The explicit jwk-set value has no text fallback: configuring it with an
// unusable value is a startup error, never a silently auth-less server.
func TestJWKSetConfigRefusedWhenUnusable(t *testing.T) {
	_, err := NewVerifier(Config{JWKSet: "this is not a key set", AnonRole: anonRole})
	if err == nil {
		t.Fatal("an unparseable jwk-set must fail startup")
	}
	_, err = NewVerifier(Config{JWKSet: `{"keys":[{"kty":"alien"}]}`, AnonRole: anonRole})
	if err == nil {
		t.Fatal("a jwk-set with an unsupported key must fail startup")
	}
}

// The jwk-set value wires real keys: a token verifies against it even with no
// jwt-secret configured.
func TestJWKSetConfigVerifies(t *testing.T) {
	secret := []byte("set-borne-secret-32-bytes-long-okay!")
	cfg := Config{JWKSet: octJWK(secret, ""), AnonRole: anonRole}
	v, err := NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.now = fixedClock(clockNow)
	tok := signWithKid(t, secret, "", jwt.MapClaims{
		"role": "web_user",
		"exp":  clockNow.Add(time.Hour).Unix(),
	})
	res, aerr := v.Authenticate("Bearer " + tok)
	if aerr != nil || res.Role != "web_user" {
		t.Fatalf("jwk-set verification: %+v, %v", res, aerr)
	}
}

// DecodeBase64Secret applies PostgREST's URL-safe character replacement before
// the standard decode and refuses undecodable values.
func TestDecodeBase64Secret(t *testing.T) {
	raw := []byte("a-secret-with-bytes-needing-urlsafe-chars???>>>")
	std := base64.StdEncoding.EncodeToString(raw)
	urlSafe := base64.URLEncoding.EncodeToString(raw)
	urlSafe = strings.ReplaceAll(urlSafe, "=", ".")

	for _, enc := range []string{std, urlSafe, "  " + std + "\n"} {
		got, err := DecodeBase64Secret(enc)
		if err != nil {
			t.Fatalf("DecodeBase64Secret(%q): %v", enc, err)
		}
		if string(got) != string(raw) {
			t.Errorf("decoded %q, want %q", got, raw)
		}
	}
	if _, err := DecodeBase64Secret("!!! not base64 !!!"); err == nil {
		t.Error("an undecodable value must error")
	}
}
