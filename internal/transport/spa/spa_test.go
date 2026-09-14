package spa

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPublicShellDeepLinksAndStaticIsolation(t *testing.T) {
	h := handler(fstest.MapFS{
		"index.html":         {Data: []byte(`<html><div id="root">Postra</div></html>`)},
		"assets/app-a1b2.js": {Data: []byte(`document.title = 'Postra';`)},
	})
	for _, tc := range []struct {
		path     string
		status   int
		contains string
	}{
		{"/app", 308, ""}, {"/app/", 200, "Postra"}, {"/app/inbox", 200, "Postra"},
		{"/app/messages/msg-123", 200, "Postra"}, {"/app/assets/app-a1b2.js", 200, "document.title"},
		{"/app/assets/missing.js", 404, ""}, {"/app/missing.css", 404, ""}, {"/app/assets/missing", 404, ""},
		{"/app/../index.html", 404, ""}, {"/app/%2e%2e/index.html", 404, ""}, {"/app/%5cindex.html", 404, ""},
		{"/app/../../private", 404, ""}, {"/app/%2e%2e/private", 404, ""},
		{"/app//index.html", 404, ""}, {"/api/messages", 404, ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.contains) {
				t.Fatalf("response %d %q; want %d containing %q", rec.Code, rec.Body.String(), tc.status, tc.contains)
			}
			if strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self' 'unsafe-inline'") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("SPA must restrict executable content to embedded same-origin scripts")
			}
			if tc.status == 200 && strings.Contains(tc.path, "/assets/") && !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
				t.Fatal("hashed static assets should be cacheable")
			}
		})
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/app/inbox", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s shell must not accept mutation: %d", method, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/app/inbox", nil))
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatal("HEAD should return shell headers only")
	}
}
