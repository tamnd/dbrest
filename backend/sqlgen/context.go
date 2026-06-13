package sqlgen

import "github.com/tamnd/dbrest/reqctx"

// ContextArgs builds the reserved :request_* placeholder values a registry
// function may reference in its SQL. On PostgreSQL a function reads the
// request context with current_setting('request.method', true); on engines
// with no SQL-readable session store the same values bind as parameters
// (spec 15), under these names:
//
//	:request_method      the HTTP method
//	:request_path        the request path
//	:request_role        the resolved request role
//	:request_jwt_claims  the verified claims as a JSON object ("{}" when none)
//	:request_headers     lower-cased request headers as a JSON object
//	:request_cookies     request cookies as a JSON object
//
// The call compiler resolves these only when the placeholder is not a
// declared parameter, so a function parameter of the same name keeps winning.
func ContextArgs(rc *reqctx.Context) map[string]any {
	if rc == nil {
		return nil
	}
	return map[string]any{
		"request_method":     rc.Method,
		"request_path":       rc.Path,
		"request_role":       rc.Role,
		"request_jwt_claims": string(rc.ClaimsJSON()),
		"request_headers":    string(rc.HeadersJSON()),
		"request_cookies":    string(rc.CookiesJSON()),
	}
}
