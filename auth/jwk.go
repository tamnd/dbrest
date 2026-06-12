package auth

// This file implements the three ways PostgREST accepts jwt-secret (spec 13):
// a literal JWK Set JSON, a single JWK JSON, or a plain text HMAC secret. The
// parsed result is always a list of verification keys; at verify time a
// token's kid selects its key and a kid-less token tries every key in turn,
// the same try-all behavior the upstream jose library applies.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// verKey is one verification key: HMAC bytes, an RSA public key, or an ECDSA
// public key, with the optional JWK kid and alg restrictions.
type verKey struct {
	kid string
	alg string
	key any // []byte, *rsa.PublicKey, or *ecdsa.PublicKey
}

// jwk is the wire form of a JSON Web Key, covering the symmetric (oct), RSA,
// and EC key types.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	K   string `json:"k"`   // oct: the key bytes
	N   string `json:"n"`   // RSA: modulus
	E   string `json:"e"`   // RSA: exponent
	Crv string `json:"crv"` // EC: curve name
	X   string `json:"x"`   // EC: x coordinate
	Y   string `json:"y"`   // EC: y coordinate
}

// parseSecretKeys parses a jwt-secret value the way PostgREST does: first as a
// JWK Set, then as a single JWK, and finally as a plain text HMAC secret. Only
// the text form carries the 32-character minimum; a malformed JSON value falls
// through to the text interpretation, matching upstream.
func parseSecretKeys(secret []byte) ([]verKey, error) {
	if keys, ok := tryJWKSet(secret); ok {
		return keys, nil
	}
	if key, ok := tryJWK(secret); ok {
		return []verKey{*key}, nil
	}
	if len(secret) < minHMACSecret {
		return nil, errors.New("jwt-secret must be at least 32 characters")
	}
	return []verKey{{key: append([]byte(nil), secret...)}}, nil
}

// parseJWKSet parses the jwk-set configuration value. Unlike jwt-secret there
// is no text fallback: the value names a key set and must be a JWK Set or a
// single JWK, otherwise startup fails rather than silently disabling auth.
func parseJWKSet(text string) ([]verKey, error) {
	b := []byte(text)
	if keys, ok := tryJWKSet(b); ok {
		return keys, nil
	}
	if key, ok := tryJWK(b); ok {
		return []verKey{*key}, nil
	}
	return nil, errors.New("not a valid JWK or JWK Set")
}

// tryJWKSet attempts to read the bytes as a {"keys": [...]} JWK Set. It only
// succeeds when every listed key is usable, the all-or-nothing reading the
// upstream JSON decoder applies.
func tryJWKSet(b []byte) ([]verKey, bool) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(b, &set); err != nil || set.Keys == nil {
		return nil, false
	}
	keys := make([]verKey, 0, len(set.Keys))
	for _, raw := range set.Keys {
		var w jwk
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, false
		}
		key, err := w.toKey()
		if err != nil {
			return nil, false
		}
		keys = append(keys, *key)
	}
	return keys, true
}

// tryJWK attempts to read the bytes as a single JWK.
func tryJWK(b []byte) (*verKey, bool) {
	var w jwk
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, false
	}
	key, err := w.toKey()
	if err != nil {
		return nil, false
	}
	return key, true
}

// toKey materializes the wire-form JWK into a verification key.
func (w jwk) toKey() (*verKey, error) {
	switch w.Kty {
	case "oct":
		k, err := b64urlDecode(w.K)
		if err != nil || len(k) == 0 {
			return nil, errors.New("oct key: bad k value")
		}
		return &verKey{kid: w.Kid, alg: w.Alg, key: k}, nil
	case "RSA":
		n, err := b64urlDecode(w.N)
		if err != nil || len(n) == 0 {
			return nil, errors.New("RSA key: bad n value")
		}
		e, err := b64urlDecode(w.E)
		if err != nil || len(e) == 0 {
			return nil, errors.New("RSA key: bad e value")
		}
		pub := &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}
		return &verKey{kid: w.Kid, alg: w.Alg, key: pub}, nil
	case "EC":
		var curve elliptic.Curve
		switch w.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("EC key: unsupported curve %q", w.Crv)
		}
		x, err := b64urlDecode(w.X)
		if err != nil || len(x) == 0 {
			return nil, errors.New("EC key: bad x value")
		}
		y, err := b64urlDecode(w.Y)
		if err != nil || len(y) == 0 {
			return nil, errors.New("EC key: bad y value")
		}
		pub := &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return nil, errors.New("EC key: point not on curve")
		}
		return &verKey{kid: w.Kid, alg: w.Alg, key: pub}, nil
	default:
		return nil, fmt.Errorf("unsupported key type %q", w.Kty)
	}
}

// b64urlDecode decodes the unpadded URL-safe base64 JWK fields use.
func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// DecodeBase64Secret decodes a jwt-secret marked with jwt-secret-is-base64.
// PostgREST replaces the URL-safe alphabet (_ to /, - to +, . to =) and strips
// whitespace before a standard base64 decode; an undecodable value is a
// startup error.
func DecodeBase64Secret(s string) ([]byte, error) {
	replaced := strings.NewReplacer("_", "/", "-", "+", ".", "=").Replace(s)
	return base64.StdEncoding.DecodeString(strings.TrimSpace(replaced))
}
