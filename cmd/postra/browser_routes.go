package main

import (
	"net/http"
	"net/url"
)

// Unknown/retired paths are not an API authentication challenge. Keep the
// fallback independent so removed UI routes really return 404 in production.
func registerAPITransports(mux *http.ServeMux, api http.Handler) {
	mux.Handle("/", http.NotFoundHandler())
	for _, pattern := range []string{"/api/", "/auth/", "/tracking/", "/healthz", "/readyz", "/livez", "/metrics"} {
		mux.Handle(pattern, api)
	}
}

// Keep only these bounded bookmarks/IdP registration bridges during v0.20.
// They contain no legacy rendering or authentication implementation and may
// be removed at v1 after operators update their registered redirect URI.
func registerBrowserRedirects(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusFound)
	})
	rootRedirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusMovedPermanently)
	}
	mux.HandleFunc("GET /ui", rootRedirect)
	mux.HandleFunc("GET /ui/{$}", rootRedirect)
	mux.HandleFunc("GET /ui/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/messages/"+url.PathEscape(r.PathValue("id")), http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /ui/auth/oidc/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		target := "/auth/oidc/callback"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect) // #nosec G710 -- scheme/host/path are fixed; opaque provider data appears only after the query delimiter
	})
}
