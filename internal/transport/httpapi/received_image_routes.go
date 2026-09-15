package httpapi

import (
	"io"
	"net/http"
)

func (s *Server) registerReceivedImageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/messages/{id}/body/frame", s.receivedBodyFrame)
	mux.HandleFunc("POST /api/messages/{id}/images/allow", s.receivedImageChoice)
	mux.HandleFunc("DELETE /api/messages/{id}/images/allow", s.receivedImageChoice)
}

// A network document has its own CSP. A srcdoc document would inherit the
// workspace's intentionally narrower img-src and prevent authorized images.
func (s *Server) receivedBodyFrame(w http.ResponseWriter, r *http.Request) {
	// A cross-site top-level link must not turn one-time UI consent into an
	// image beacon. Only the workspace's same-origin iframe may use this flag.
	once := r.URL.Query().Get("external_images") == "once" && r.Header.Get("Sec-Fetch-Site") == "same-origin" && r.Header.Get("Sec-Fetch-Dest") == "iframe"
	view, err := s.app.GetReceivedMessage(r.Context(), r.PathValue("id"), once)
	if err != nil {
		writeErr(w, err)
		return
	}
	if view.Body == nil || view.Body.HTMLSanitized == "" || view.Body.Unavailable {
		http.NotFound(w, r)
		return
	}
	images := "'none'"
	if view.Body.ImagesAllowed {
		images = "https: http:"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src "+images+"; style-src 'unsafe-inline'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'")
	_, _ = io.WriteString(w, `<!doctype html><html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="referrer" content="no-referrer"><style>body{margin:0;padding:20px;background:white;color:#172033;font:15px/1.7 Arial,sans-serif;overflow-wrap:anywhere}table,img{max-width:100%}a{color:#3157d5}blockquote{border-left:3px solid #dfe5ee;margin-left:0;padding-left:16px}pre{white-space:pre-wrap}</style></head><body>`+view.Body.HTMLSanitized+`</body></html>`) // #nosec G705 -- application re-sanitizes untrusted received HTML; opaque CSP sandbox forbids script execution
}

func (s *Server) receivedImageChoice(w http.ResponseWriter, r *http.Request) {
	input, err := decode[struct {
		Scope string `json:"scope"`
	}](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.AllowReceivedImages(r.Context(), r.PathValue("id"), input.Scope, r.Method == http.MethodDelete); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
