// Package auth verifies JSON Web Tokens and resolves the request role, the
// single piece of PostgREST's stateless auth model that lives in the frontend
// (spec 13). It is backend-agnostic: the signature and algorithm checks, the
// exp/nbf/iat/aud validation, the role resolution, and the PGRST301/302/303
// codes are produced here and are byte-identical on every engine. Only the
// unobservable role switch differs per backend, which this package never touches.
//
// This is a leaf package over a standard audited JWT library; nothing in the
// rest of dbrest is imported except the shared error envelope.
package auth

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tamnd/dbrest/pgerr"
)

// defaultSkew is the clock-skew tolerance PostgREST applies to the time claims.
const defaultSkew = 30 * time.Second

// minHMACSecret is the shortest HMAC secret accepted, matching PostgREST v13.0
// onward: a shorter secret is a configuration error caught at startup, never a
// runtime 401.
const minHMACSecret = 32

// Config declares how tokens are verified and how the role is read. The values
// come from the configuration layer (spec 20); this package applies only the
// defaults whose zero value is unambiguous (the skew and the role-claim key).
type Config struct {
	// AllowedAlgs pins the accepted alg header values (HS256, RS256, ES384, ...).
	// Empty derives the set from the configured keys. "none" is never accepted and
	// is rejected if listed explicitly.
	AllowedAlgs []string
	// Secret is the shared HMAC secret. When set it must be at least 32 bytes.
	Secret []byte
	// PublicKeyPEM is a PEM-encoded RSA or ECDSA public key (the static key source
	// for the asymmetric families).
	PublicKeyPEM string
	// Audience, when set, must appear in the token's aud claim.
	Audience string
	// RoleClaimKey names the claim the request role is read from; default ".role".
	// The value is a JSPath expression: dotted keys (".app_metadata.role"), quoted
	// keys (."https://example.com/role"), array indexes (".roles[0]"), and a
	// trailing filter (".roles[?(@ == \"admin\")]"). An invalid value is a
	// startup error.
	RoleClaimKey string
	// AnonRole is the role an unauthenticated or role-less request runs as. Empty
	// means such requests are refused rather than run as the connection identity.
	AnonRole string
	// PermittedRoles, when non-empty, is the set of roles the authenticator may
	// assume; a valid token naming a role outside it gets a 403. Empty defers the
	// check to the authorization layer (spec 14). The anon role is always allowed.
	PermittedRoles []string
	// Skew is the clock-skew tolerance for exp/nbf/iat; default 30s.
	Skew time.Duration
	// CacheMaxEntries bounds the verified-token cache: a value greater than zero
	// enables a SIEVE-evicted cache of that size, zero disables it (spec 13). The
	// default of 1000 is applied by the configuration layer, not here.
	CacheMaxEntries int
}

// Result is the resolved identity of a request: the role it runs as, the
// verified claims (nil when no token was presented), and whether it is anonymous.
type Result struct {
	Role      string
	Claims    map[string]any
	Anonymous bool
}

// Verifier verifies tokens against a fixed configuration. It is safe for
// concurrent use: the verification is stateless and the optional cache guards
// itself.
type Verifier struct {
	validMethods []string
	hmac         []byte
	rsa          *rsa.PublicKey
	ecdsa        *ecdsa.PublicKey
	hasKeys      bool

	audience    string
	roleKeyPath []jsPathExp
	anonRole    string
	permitted   map[string]bool
	skew        time.Duration

	cache *jwtCache
	now   func() time.Time
}

// NewVerifier builds a Verifier from a Config, applying the unambiguous defaults
// and validating the key material. It fails fast on a configuration error: an
// HMAC secret shorter than 32 bytes, a "none" in the allowed algorithms, or an
// unparseable public key. The secret itself is never echoed in any error.
func NewVerifier(cfg Config) (*Verifier, error) {
	v := &Verifier{
		audience:  cfg.Audience,
		anonRole:  cfg.AnonRole,
		skew:      cfg.Skew,
		now:       time.Now,
		permitted: map[string]bool{},
	}
	if v.skew == 0 {
		v.skew = defaultSkew
	}
	for _, r := range cfg.PermittedRoles {
		v.permitted[r] = true
	}
	roleKey, err := parseJSPath(cfg.RoleClaimKey)
	if err != nil {
		return nil, err
	}
	v.roleKeyPath = roleKey

	if len(cfg.Secret) > 0 {
		if len(cfg.Secret) < minHMACSecret {
			return nil, errors.New("jwt-secret must be at least 32 characters")
		}
		v.hmac = cfg.Secret
		v.hasKeys = true
	}
	if cfg.PublicKeyPEM != "" {
		if err := v.loadPublicKey(cfg.PublicKeyPEM); err != nil {
			return nil, err
		}
		v.hasKeys = true
	}

	methods, err := v.resolveMethods(cfg.AllowedAlgs)
	if err != nil {
		return nil, err
	}
	v.validMethods = methods

	if cfg.CacheMaxEntries > 0 {
		v.cache = newJWTCache(cfg.CacheMaxEntries)
	}
	return v, nil
}

// Authenticate resolves the identity of a request from its Authorization header
// value. No bearer token runs as anon; a token that cannot be decoded is
// PGRST301; a decoded token failing claims validation is PGRST303; a valid
// token naming a forbidden role is 403.
// When no key material is configured the server fails closed, as PostgREST
// does: a presented token is a 500 PGRST300, never silently accepted.
func (v *Verifier) Authenticate(authHeader string) (*Result, *pgerr.APIError) {
	raw, ok := bearer(authHeader)
	if !ok {
		return v.anon()
	}
	if !v.hasKeys {
		return nil, pgerr.ErrJWTSecretMissing()
	}

	if v.cache != nil {
		if claims, hit := v.cache.get(raw); hit {
			// A cached entry never extends a token's life: the time claims are
			// re-checked against the live clock on every request (spec 13).
			if apiErr := v.checkTime(claims); apiErr != nil {
				return nil, apiErr
			}
			return v.resolve(claims)
		}
	}

	claims, apiErr := v.verify(raw)
	if apiErr != nil {
		return nil, apiErr
	}
	if v.cache != nil {
		v.cache.put(raw, claims)
	}
	return v.resolve(claims)
}

// verify checks the signature, the pinned algorithm, and the time and audience
// claims with skew, returning the claim set or a JWT error. The error messages
// are PostgREST's fixed texts: the token and the secret are never reflected
// back to the client.
func (v *Verifier) verify(raw string) (map[string]any, *pgerr.APIError) {
	if n := strings.Count(raw, ".") + 1; n != 3 {
		return nil, pgerr.ErrJWTDecode(fmt.Sprintf("Expected 3 parts in JWT; got %d", n))
	}
	if apiErr := v.checkAlg(raw); apiErr != nil {
		return nil, apiErr
	}
	claims := jwt.MapClaims{}
	opts := []jwt.ParserOption{
		jwt.WithValidMethods(v.validMethods),
		jwt.WithLeeway(v.skew),
		jwt.WithTimeFunc(v.now),
	}
	if v.audience != "" {
		opts = append(opts, jwt.WithAudience(v.audience))
	}
	if _, err := jwt.NewParser(opts...).ParseWithClaims(raw, claims, v.keyfunc); err != nil {
		return nil, mapJWTError(err)
	}
	return map[string]any(claims), nil
}

// checkAlg reads the unverified alg header of a compact JWT and rejects a value
// outside the pinned method set before any cryptography runs. The three failure
// shapes carry PostgREST's exact messages: an unsecured token, an alg the
// library does not know, and a known alg with no matching key.
func (v *Verifier) checkAlg(raw string) *pgerr.APIError {
	headerPart := raw[:strings.IndexByte(raw, '.')]
	headerJSON, err := base64.RawURLEncoding.DecodeString(headerPart)
	if err != nil {
		return pgerr.ErrJWTDecode("JWT cryptographic operation failed")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return pgerr.ErrJWTDecode("JWT cryptographic operation failed")
	}
	if strings.EqualFold(header.Alg, "none") {
		return pgerr.ErrJWTDecode("Wrong or unsupported encoding algorithm").
			WithDetails("JWT is unsecured but expected 'alg' was not 'none'")
	}
	if jwt.GetSigningMethod(header.Alg) == nil {
		return pgerr.ErrJWTDecode("JWT cryptographic operation failed")
	}
	for _, m := range v.validMethods {
		if header.Alg == m {
			return nil
		}
	}
	return pgerr.ErrJWTDecode("No suitable key or wrong key type").
		WithDetails("No suitable key was found to decode the JWT")
}

// mapJWTError translates a golang-jwt failure onto the v14 code split: claim
// validation failures are PGRST303, everything that prevented decoding or
// verifying the token is PGRST301. The messages are PostgREST's own.
func mapJWTError(err error) *pgerr.APIError {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return pgerr.ErrJWTClaims("JWT expired")
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return pgerr.ErrJWTClaims("JWT not yet valid")
	case errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		return pgerr.ErrJWTClaims("JWT issued at future")
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return pgerr.ErrJWTClaims("JWT not in audience")
	case errors.Is(err, jwt.ErrTokenInvalidClaims):
		return pgerr.ErrJWTClaims("Parsing claims failed")
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return pgerr.ErrJWTDecode("No suitable key or wrong key type").
			WithDetails("None of the keys was able to decode the JWT")
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		return pgerr.ErrJWTDecode("No suitable key or wrong key type").
			WithDetails("No suitable key was found to decode the JWT")
	default:
		return pgerr.ErrJWTDecode("JWT cryptographic operation failed")
	}
}

// keyfunc returns the verification key for the token's algorithm family. The
// allowed-methods parser option already blocks a disallowed alg before this runs,
// so the algorithm-confusion swap (an RS token verified against an HMAC secret)
// cannot reach a key of the wrong family.
func (v *Verifier) keyfunc(t *jwt.Token) (any, error) {
	switch t.Method.(type) {
	case *jwt.SigningMethodHMAC:
		if v.hmac == nil {
			return nil, errors.New("no HMAC key configured")
		}
		return v.hmac, nil
	case *jwt.SigningMethodRSA:
		if v.rsa == nil {
			return nil, errors.New("no RSA key configured")
		}
		return v.rsa, nil
	case *jwt.SigningMethodECDSA:
		if v.ecdsa == nil {
			return nil, errors.New("no ECDSA key configured")
		}
		return v.ecdsa, nil
	default:
		return nil, errors.New("unsupported signing method")
	}
}

// checkTime re-validates the exp and nbf claims against the live clock with the
// configured skew. It runs on a cache hit so a cached verification can never
// resurrect an expired token.
func (v *Verifier) checkTime(claims map[string]any) *pgerr.APIError {
	now := v.now()
	if exp, ok := numClaim(claims, "exp"); ok {
		if now.After(time.Unix(exp, 0).Add(v.skew)) {
			return pgerr.ErrJWTClaims("JWT expired")
		}
	}
	if nbf, ok := numClaim(claims, "nbf"); ok {
		if now.Before(time.Unix(nbf, 0).Add(-v.skew)) {
			return pgerr.ErrJWTClaims("JWT not yet valid")
		}
	}
	return nil
}

// resolve reads the role from the claims and applies the anon fallback and the
// permitted-role check. A valid token that resolves to no role and has no anon
// fallback is refused; a role outside the permitted set is a 403.
func (v *Verifier) resolve(claims map[string]any) (*Result, *pgerr.APIError) {
	role := roleFromClaims(claims, v.roleKeyPath)
	if role == "" {
		role = v.anonRole
	}
	if role == "" {
		return nil, errAnonDisabled()
	}
	if apiErr := v.checkPermitted(role); apiErr != nil {
		return nil, apiErr
	}
	return &Result{Role: role, Claims: claims, Anonymous: false}, nil
}

// checkPermitted enforces the optional permitted-role set. The anon role is
// always allowed; an empty set defers the decision to the authorization layer.
func (v *Verifier) checkPermitted(role string) *pgerr.APIError {
	if role == v.anonRole || len(v.permitted) == 0 || v.permitted[role] {
		return nil
	}
	return pgerr.ErrRoleNotAllowed(role)
}

// anon resolves an unauthenticated request to the anon role, or refuses it when
// no anon role is configured.
func (v *Verifier) anon() (*Result, *pgerr.APIError) {
	if v.anonRole == "" {
		return nil, errAnonDisabled()
	}
	return &Result{Role: v.anonRole, Anonymous: true}, nil
}

// errAnonDisabled is the 401 a request gets when it presents no usable identity
// and no anon role is configured, so it cannot be run as anyone. The message is
// PostgREST's exact PGRST302 text.
func errAnonDisabled() *pgerr.APIError {
	return pgerr.ErrJWTRequired()
}

// loadPublicKey parses a PEM-encoded RSA or ECDSA public key into the verifier.
func (v *Verifier) loadPublicKey(pemText string) error {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return errors.New("jwt public key is not valid PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return errors.New("jwt public key is not a valid PKIX key")
	}
	switch k := key.(type) {
	case *rsa.PublicKey:
		v.rsa = k
	case *ecdsa.PublicKey:
		v.ecdsa = k
	default:
		return errors.New("jwt public key is neither RSA nor ECDSA")
	}
	return nil
}

// resolveMethods returns the pinned alg set. A configured list wins (with "none"
// rejected); otherwise the families of the configured keys are allowed.
func (v *Verifier) resolveMethods(allowed []string) ([]string, error) {
	if len(allowed) > 0 {
		for _, a := range allowed {
			if strings.EqualFold(a, "none") {
				return nil, errors.New("the none algorithm is not accepted")
			}
		}
		return allowed, nil
	}
	var methods []string
	if v.hmac != nil {
		methods = append(methods, "HS256", "HS384", "HS512")
	}
	if v.rsa != nil {
		methods = append(methods, "RS256", "RS384", "RS512")
	}
	if v.ecdsa != nil {
		methods = append(methods, "ES256", "ES384", "ES512")
	}
	return methods, nil
}

// bearer extracts the token from an Authorization header value, accepting the
// "Bearer" scheme case-insensitively. It reports false for any other header.
func bearer(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(scheme):])
	return tok, tok != ""
}

// roleFromClaims walks the role-claim JSPath over the claim set and returns the
// string value it resolves to, or "" if the path resolves to nothing or to a
// non-string value.
func roleFromClaims(claims map[string]any, path []jsPathExp) string {
	val, ok := walkJSPath(claims, path)
	if !ok {
		return ""
	}
	if s, ok := val.(string); ok {
		return s
	}
	return ""
}

// numClaim reads a numeric claim as a Unix-seconds int64, handling the float64
// and json.Number forms a decoded claim set can carry.
func numClaim(claims map[string]any, name string) (int64, bool) {
	switch t := claims[name].(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
	}
	return 0, false
}
