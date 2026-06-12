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
	// Secret is the jwt-secret value. As in PostgREST it is read three ways: a
	// JWK Set JSON, a single JWK JSON, or a plain text HMAC secret (which must
	// be at least 32 bytes).
	Secret []byte
	// JWKSet is an explicit JWK Set (or single JWK) JSON. Unlike Secret it has
	// no text fallback: an unparseable value is a startup error.
	JWKSet string
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
	keys         []verKey
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
		keys, err := parseSecretKeys(cfg.Secret)
		if err != nil {
			return nil, err
		}
		v.keys = append(v.keys, keys...)
	}
	if cfg.JWKSet != "" {
		keys, err := parseJWKSet(cfg.JWKSet)
		if err != nil {
			return nil, fmt.Errorf("jwk-set: %w", err)
		}
		v.keys = append(v.keys, keys...)
	}
	if cfg.PublicKeyPEM != "" {
		if err := v.loadPublicKey(cfg.PublicKeyPEM); err != nil {
			return nil, err
		}
	}
	v.hasKeys = len(v.keys) > 0

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
	if raw == "" {
		// A bearer scheme with nothing after it is a malformed credential, not
		// an anonymous request: PostgREST answers PGRST301 with this message.
		return nil, pgerr.ErrJWTDecode("Empty JWT is sent in Authorization header")
	}
	if !v.hasKeys {
		return nil, pgerr.ErrJWTSecretMissing()
	}

	if v.cache != nil {
		if claims, hit := v.cache.get(raw); hit {
			// A cached entry never extends a token's life: the claims are
			// re-validated against the live clock on every request (spec 13).
			if apiErr := v.validateClaims(claims); apiErr != nil {
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
	// Claims validation is done by validateClaims below, not by the library:
	// PostgREST's rules differ (an absent or empty aud passes, iat is checked,
	// and the type errors carry their own PGRST303 messages).
	opts := []jwt.ParserOption{
		jwt.WithValidMethods(v.validMethods),
		jwt.WithoutClaimsValidation(),
	}
	if _, err := jwt.NewParser(opts...).ParseWithClaims(raw, claims, v.keyfunc); err != nil {
		return nil, mapJWTError(err)
	}
	if apiErr := v.validateClaims(claims); apiErr != nil {
		return nil, apiErr
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

// keyfunc selects the verification keys for a token. A kid header narrows the
// set to the keys carrying that kid; a kid-less token tries every key of the
// right family in turn, as the upstream jose library does. The allowed-methods
// parser option already blocks a disallowed alg before this runs, so the
// algorithm-confusion swap (an RS token verified against an HMAC secret)
// cannot reach a key of the wrong family.
func (v *Verifier) keyfunc(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)
	set := jwt.VerificationKeySet{}
	for _, k := range v.keys {
		if kid != "" && k.kid != kid {
			continue
		}
		if k.alg != "" && k.alg != t.Method.Alg() {
			continue
		}
		if !methodMatchesKey(t.Method, k.key) {
			continue
		}
		set.Keys = append(set.Keys, k.key)
	}
	switch len(set.Keys) {
	case 0:
		return nil, errors.New("no suitable key was found to decode the JWT")
	case 1:
		return set.Keys[0], nil
	default:
		return set, nil
	}
}

// methodMatchesKey reports whether a verification key belongs to the family of
// a signing method.
func methodMatchesKey(method jwt.SigningMethod, key any) bool {
	switch method.(type) {
	case *jwt.SigningMethodHMAC:
		_, ok := key.([]byte)
		return ok
	case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
		_, ok := key.(*rsa.PublicKey)
		return ok
	case *jwt.SigningMethodECDSA:
		_, ok := key.(*ecdsa.PublicKey)
		return ok
	}
	return false
}

// validateClaims applies PostgREST's claim checks in its order: exp, nbf, iat,
// then aud, each with the 30 second skew. An absent or null claim passes; a
// present claim of the wrong type is its own PGRST303 error. It runs on every
// request, including cache hits, so a cached verification can never resurrect
// an expired token.
func (v *Verifier) validateClaims(claims map[string]any) *pgerr.APIError {
	now := v.now().Unix()
	skew := int64(v.skew / time.Second)

	if val, ok := presentClaim(claims, "exp"); ok {
		exp, isNum := claimNumber(val)
		if !isNum {
			return pgerr.ErrJWTClaims("The JWT 'exp' claim must be a number")
		}
		if now-skew > exp {
			return pgerr.ErrJWTClaims("JWT expired")
		}
	}
	if val, ok := presentClaim(claims, "nbf"); ok {
		nbf, isNum := claimNumber(val)
		if !isNum {
			return pgerr.ErrJWTClaims("The JWT 'nbf' claim must be a number")
		}
		if now+skew < nbf {
			return pgerr.ErrJWTClaims("JWT not yet valid")
		}
	}
	if val, ok := presentClaim(claims, "iat"); ok {
		iat, isNum := claimNumber(val)
		if !isNum {
			return pgerr.ErrJWTClaims("The JWT 'iat' claim must be a number")
		}
		if now+skew < iat {
			return pgerr.ErrJWTClaims("JWT issued at future")
		}
	}
	if val, ok := presentClaim(claims, "aud"); ok {
		if apiErr := v.checkAud(val); apiErr != nil {
			return apiErr
		}
	}
	return nil
}

// checkAud validates the aud claim the PostgREST way: a string must match the
// configured audience, an array passes when empty or when any element matches,
// and anything else is a type error. With no jwt-aud configured every audience
// matches.
func (v *Verifier) checkAud(val any) *pgerr.APIError {
	switch aud := val.(type) {
	case string:
		if !v.audMatches(aud) {
			return pgerr.ErrJWTClaims("JWT not in audience")
		}
	case []any:
		matched := len(aud) == 0
		for _, el := range aud {
			s, isStr := el.(string)
			if !isStr {
				return pgerr.ErrJWTClaims("The JWT 'aud' claim must be a string or an array of strings")
			}
			if v.audMatches(s) {
				matched = true
			}
		}
		if !matched {
			return pgerr.ErrJWTClaims("JWT not in audience")
		}
	default:
		return pgerr.ErrJWTClaims("The JWT 'aud' claim must be a string or an array of strings")
	}
	return nil
}

// audMatches reports whether a token audience satisfies the configured jwt-aud.
// An unset jwt-aud accepts every audience.
func (v *Verifier) audMatches(aud string) bool {
	return v.audience == "" || v.audience == aud
}

// resolve reads the role from the claims and applies the anon fallback and the
// permitted-role check. Only a genuinely absent role claim falls back to the
// anonymous role: a present claim of any other type is rendered to text and
// used as the role name, exactly as PostgREST does (the engine or the authz
// registry then denies a role that does not exist, rather than the client
// being silently downgraded to anonymous data). A valid token that resolves
// to no role and has no anon fallback is refused; a role outside the
// permitted set is a 403.
func (v *Verifier) resolve(claims map[string]any) (*Result, *pgerr.APIError) {
	role, present := roleFromClaims(claims, v.roleKeyPath)
	if !present {
		role = v.anonRole
		if role == "" {
			return nil, errAnonDisabled()
		}
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
	case *rsa.PublicKey, *ecdsa.PublicKey:
		v.keys = append(v.keys, verKey{key: k})
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
	var hmac, rsaKey, ecdsaKey bool
	for _, k := range v.keys {
		switch k.key.(type) {
		case []byte:
			hmac = true
		case *rsa.PublicKey:
			rsaKey = true
		case *ecdsa.PublicKey:
			ecdsaKey = true
		}
	}
	var methods []string
	if hmac {
		methods = append(methods, "HS256", "HS384", "HS512")
	}
	if rsaKey {
		methods = append(methods, "RS256", "RS384", "RS512", "PS256", "PS384", "PS512")
	}
	if ecdsaKey {
		methods = append(methods, "ES256", "ES384", "ES512")
	}
	return methods, nil
}

// bearer extracts the token from an Authorization header value, mirroring the
// wai-extra extractBearerAuth PostgREST uses: the first whitespace ends the
// scheme word, the comparison is case-insensitive, and the token is whatever
// follows the leading whitespace, possibly empty. It reports false only when
// the credentials are not a bearer scheme at all, which is the anonymous
// path; "Bearer" with an empty token reports true so the caller can answer
// PGRST301 instead of downgrading the client to anon.
func bearer(header string) (string, bool) {
	scheme, rest := header, ""
	if i := strings.IndexAny(header, " \t"); i >= 0 {
		scheme, rest = header[:i], header[i+1:]
	}
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimLeft(rest, " \t"), true
}

// roleFromClaims walks the role-claim JSPath over the claim set, reporting
// whether the path resolved to a value at all. A resolved value is rendered
// the way PostgREST renders a claim where text is expected: a string is taken
// bare, anything else (a number, bool, null, array, or object) becomes its
// compact JSON text and is used as the role name verbatim.
func roleFromClaims(claims map[string]any, path []jsPathExp) (string, bool) {
	val, ok := walkJSPath(claims, path)
	if !ok {
		return "", false
	}
	return unquoted(val), true
}

// unquoted renders a claim value as the text PostgREST would use it as: a
// string stays bare, every other JSON value is its compact rendering ("null",
// "42", "true", "[\"a\"]").
func unquoted(val any) string {
	if s, ok := val.(string); ok {
		return s
	}
	b, err := json.Marshal(val)
	if err != nil {
		return fmt.Sprint(val)
	}
	return string(b)
}

// presentClaim reports a claim's value when it is present and non-null. An
// absent or null claim is skipped by every check, as upstream.
func presentClaim(claims map[string]any, name string) (any, bool) {
	val, ok := claims[name]
	if !ok || val == nil {
		return nil, false
	}
	return val, true
}

// claimNumber reads a numeric claim value as Unix seconds, handling the
// float64 and json.Number forms a decoded claim set can carry. A non-number is
// reported false and becomes the claim's PGRST303 type error.
func claimNumber(val any) (int64, bool) {
	switch t := val.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}
