package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowserRetirementRedirects(t *testing.T) {
	mux := http.NewServeMux()
	registerAPITransports(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	registerBrowserRedirects(mux)
	for _, tc := range []struct {
		path     string
		code     int
		location string
	}{
		{"/", 302, "/app/"},
		{"/ui", 301, "/app/"},
		{"/ui/", 301, "/app/"},
		{"/ui/messages/m1", 301, "/app/messages/m1"},
		{"/ui/auth/oidc/callback?code=fixture%2Bcode&state=fixture-state", 307, "/auth/oidc/callback?code=fixture%2Bcode&state=fixture-state"},
		{"/ui/auth/oidc/callback?return_to=https://evil.test/%0d%0aLocation:%20https://evil.test&state=//evil.test", 307, "/auth/oidc/callback?return_to=https://evil.test/%0d%0aLocation:%20https://evil.test&state=//evil.test"},
		{"/ui/login", 404, ""},
		{"/ui/admin/settings", 404, ""},
		{"/ui/static/mail-editor.js", 404, ""},
		{"/api/v1/me", 401, ""},
		{"/api/me", 401, ""},
		{"/unknown", 404, ""},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.code || w.Header().Get("Location") != tc.location {
			t.Errorf("%s: got %d %q, want %d %q", tc.path, w.Code, w.Header().Get("Location"), tc.code, tc.location)
		}
		if tc.code == 307 && (w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer") {
			t.Fatal("callback credentials must not be cached or sent as a referrer")
		}
	}
}
