package httpapi

import (
	"net/http"
	"testing"

	"github.com/tamnd/dbrest/pgerr"
)

// 04.7: a native 42501 insufficient_privilege is 403 for an authenticated
// request and 401 (with a Bearer challenge) for an anonymous one. The base
// error carries 403; mapExecError lifts only the anonymous case.

func TestMapExecError42501Split(t *testing.T) {
	base := pgerr.New(http.StatusForbidden, pgerr.CodeInsufficientPrivilege, "permission denied for table films")

	authed := mapExecError(nil, base, false)
	if authed.HTTPStatus != http.StatusForbidden {
		t.Errorf("authenticated status = %d, want 403", authed.HTTPStatus)
	}
	if authed.WWWAuthenticate != "" {
		t.Errorf("authenticated WWW-Authenticate = %q, want none", authed.WWWAuthenticate)
	}

	anon := mapExecError(nil, base, true)
	if anon.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want 401", anon.HTTPStatus)
	}
	if anon.WWWAuthenticate != "Bearer" {
		t.Errorf("anonymous WWW-Authenticate = %q, want Bearer", anon.WWWAuthenticate)
	}
	// The lift must not mutate the shared base error.
	if base.HTTPStatus != http.StatusForbidden {
		t.Errorf("base mutated to %d", base.HTTPStatus)
	}
}
