package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"postra/internal/domain"
	"postra/internal/platform/tracking"
)

const trackingFramePath = "/api/tracking/frame"
const trackingReportPath = "/tracking/csp-report"

// Only coarse route names enter telemetry. Message/account IDs, search terms,
// credentials, and all personal-setting/authentication routes are excluded.
func trackingPage(raw string) string {
	switch raw {
	case "/app/mail", "/app/search", "/app/work", "/app/team", "/app/actions", "/app/ask", "/app/digest", "/app/rules", "/app/sent", "/app/drafts", "/app/compose", "/app/accounts", "/app/jobs", "/app/admin", "/app/messages/:id", "/app/threads/:id", "/app/drafts/:id", "/app/accounts/:id", "/app/jobs/:id":
		return raw
	}
	return ""
}

func (s *Server) registerTrackingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tracking", s.browserTracking)
	mux.HandleFunc("GET "+trackingFramePath, s.trackingFrame)
	mux.HandleFunc("POST "+trackingReportPath, s.trackingReport)
	mux.HandleFunc("GET /api/admin/tracking", s.adminTracking)
	mux.HandleFunc("POST /api/admin/tracking/allow", s.allowTrackingOrigin)
	mux.HandleFunc("DELETE /api/admin/tracking/violations", s.forgetTrackingViolations)
	mux.HandleFunc(tracking.ProxyPath+"/", s.trackingProxy)
}

func (s *Server) browserTracking(w http.ResponseWriter, r *http.Request) {
	page := trackingPage(r.URL.Query().Get("page"))
	config := s.app.TrackingConfig(r.Context())
	response := map[string]any{"active": page != "" && config.Active(page)}
	if response["active"] == true {
		response["frame_url"] = trackingFramePath + "?page=" + url.QueryEscape(page)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) trackingFrame(w http.ResponseWriter, r *http.Request) {
	page := trackingPage(r.URL.Query().Get("page"))
	config := s.app.TrackingConfig(r.Context())
	if page == "" || !config.Active(page) {
		http.NotFound(w, r)
		return
	}
	var entropy [18]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		writeErr(w, err)
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(entropy[:])
	scripts, connects, images := config.PolicySources()
	policy := "default-src 'none'; sandbox allow-scripts; script-src 'self' 'nonce-" + nonce + "' " + strings.Join(scripts, " ") + "; connect-src 'self' " + strings.Join(connects, " ") + "; img-src 'self' data: " + strings.Join(images, " ") + "; style-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; report-uri " + trackingReportPath
	w.Header().Set("Content-Security-Policy", policy)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The administrator-controlled snippet executes only in an opaque sandbox:
	// never allow-same-origin, never the mail/login document, never localStorage.
	snippet := config.Snippet(nonce)
	head, body := "", ""
	if config.Placement == "body" {
		body = snippet
	} else {
		head = snippet
	}
	_, _ = io.WriteString(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>"+html.EscapeString(page)+"</title>"+head+"</head><body>"+body+"</body></html>") // #nosec G705 -- intentional administrator-authored tracker, isolated by opaque sandbox CSP and iframe; never embedded in parent document
}

func (s *Server) trackingReport(w http.ResponseWriter, r *http.Request) {
	defer w.WriteHeader(http.StatusNoContent)
	var in struct {
		Report struct {
			Blocked   string `json:"blocked-uri"`
			Effective string `json:"effective-directive"`
			Violated  string `json:"violated-directive"`
			Document  string `json:"document-uri"`
		} `json:"csp-report"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024)).Decode(&in) != nil {
		return
	}
	parsed, err := url.Parse(in.Report.Document)
	if err != nil || parsed.Path != trackingFramePath {
		return
	}
	page := trackingPage(parsed.Query().Get("page"))
	if page == "" {
		return
	}
	directive := in.Report.Effective
	if directive == "" {
		directive = in.Report.Violated
	}
	s.trackingViolations.Record(in.Report.Blocked, directive, page)
}

func (s *Server) adminTracking(w http.ResponseWriter, r *http.Request) {
	if _, err := s.app.AdminSettingsCatalog(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	config := s.app.TrackingConfig(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"violations": s.trackingViolations.List(config), "enabled": config.Enabled, "provider": config.Provider, "isolated": true})
}
func (s *Server) allowTrackingOrigin(w http.ResponseWriter, r *http.Request) {
	in, err := decode[struct {
		Origin string `json:"origin"`
	}](r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := s.app.AdminAllowTrackingHost(r.Context(), in.Origin); err != nil {
		writeErr(w, err)
		return
	}
	s.adminTracking(w, r)
}
func (s *Server) forgetTrackingViolations(w http.ResponseWriter, r *http.Request) {
	if _, err := s.app.AdminSettingsCatalog(r.Context()); err != nil {
		writeErr(w, err)
		return
	}
	s.trackingViolations.Forget()
	s.adminTracking(w, r)
}

func (s *Server) trackingProxy(w http.ResponseWriter, r *http.Request) {
	config := s.app.TrackingConfig(r.Context())
	if !config.ProxyEnabled() {
		http.NotFound(w, r)
		return
	}
	target, err := url.Parse(strings.TrimRight(strings.TrimSpace(config.MomentoURL), "/"))
	if err != nil || target.Host == "" || target.User != nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodOptions {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// An opaque tracker may send anonymous events, but never credentialed CORS.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + strings.TrimPrefix(r.URL.Path, tracking.ProxyPath)
		pr.Out.URL.RawPath = ""
		for _, name := range []string{"Cookie", "Authorization", "X-CSRF-Token", "Referer", "Proxy-Authorization"} {
			pr.Out.Header.Del(name)
		}
	}, ModifyResponse: func(response *http.Response) error {
		response.Header.Del("Set-Cookie")
		response.Header.Del("Access-Control-Allow-Credentials")
		response.Header.Set("Access-Control-Allow-Origin", "*")
		return nil
	}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		writeErr(w, &domain.PublicError{Code: "upstream_unavailable", Message: "추적 수집기에 연결할 수 없습니다.", Status: http.StatusBadGateway})
	}}
	proxy.ServeHTTP(w, r)
}
