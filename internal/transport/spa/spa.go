// Package spa serves the self-contained browser application. Browser identity
// remains in the shared HttpOnly session cookie; the shell contains no secrets.
package spa

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// Vite builds with base /app/ into this directory before the Go release build.
//
//go:embed all:assets
var files embed.FS

func Handler() http.Handler {
	assets, _ := fs.Sub(files, "assets")
	return handler(assets)
}

func handler(assets fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; frame-src 'self' blob:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/app" {
			http.Redirect(w, r, "/app/", http.StatusPermanentRedirect)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/app/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/app/")
		if strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || (name != "" && (!fs.ValidPath(strings.TrimSuffix(name, "/")) || path.Clean(name) != strings.TrimSuffix(name, "/"))) {
			http.NotFound(w, r)
			return
		}
		if name == "" {
			name = "index.html"
		}
		data, err := fs.ReadFile(assets, name)
		if err != nil {
			// Deep links may use the shell, but missing assets and traversal
			// attempts must never receive HTML in place of JavaScript/CSS.
			if strings.HasPrefix(name, "assets/") || strings.Contains(path.Base(name), ".") {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
			data, err = fs.ReadFile(assets, name)
			if err != nil {
				http.Error(w, "browser application unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		contentType := mime.TypeByExtension(path.Ext(name))
		// Historical brand files have a .png suffix but contain JPEG bytes.
		// Detect embedded images without changing or trusting any remote data.
		if strings.HasPrefix(contentType, "image/") {
			contentType = http.DetectContentType(data)
		}
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		if r.Method != http.MethodHead {
			_, _ = w.Write(data) // #nosec G705 -- embedded static assets with an explicit MIME type
		}
	})
}
