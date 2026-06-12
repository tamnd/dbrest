package httpapi_test

import (
	"net/http"
	"testing"
)

// TestContextCarriesConfiguredSettings checks that db-pre-request,
// app.settings.*, and log-query reach the backend on the per-request context,
// the seam each driver consumes them from.
func TestContextCarriesConfiguredSettings(t *testing.T) {
	srv, cb := captureServer(t)
	srv.SetPreRequest("check_request")
	srv.SetAppSettings(map[string]string{"tenant": "acme"})
	srv.SetLogQuery(true)

	srv.ServeHTTP(newRecorder(), newReq(http.MethodGet, "/films"))
	if cb.got == nil {
		t.Fatal("backend never saw a request context")
	}
	if cb.got.PreRequest != "check_request" {
		t.Errorf("PreRequest = %q, want check_request", cb.got.PreRequest)
	}
	if cb.got.AppSettings["tenant"] != "acme" {
		t.Errorf("AppSettings = %v, want tenant=acme", cb.got.AppSettings)
	}
	if !cb.got.LogQuery {
		t.Error("LogQuery did not reach the backend")
	}
}

// TestContextSettingsUnsetByDefault pins the unconfigured shape: no hook, no
// settings, no echo on the context.
func TestContextSettingsUnsetByDefault(t *testing.T) {
	srv, cb := captureServer(t)
	srv.ServeHTTP(newRecorder(), newReq(http.MethodGet, "/films"))
	if cb.got == nil {
		t.Fatal("backend never saw a request context")
	}
	if cb.got.PreRequest != "" || len(cb.got.AppSettings) != 0 || cb.got.LogQuery {
		t.Errorf("unconfigured context carries settings: pre=%q app=%v logQuery=%v",
			cb.got.PreRequest, cb.got.AppSettings, cb.got.LogQuery)
	}
}
