package webui

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"postra/internal/application"
	"postra/internal/platform/tracking"
)

// cspReportPath receives the browser's policy violation reports while
// tracking is on. It is unauthenticated: the browser posts there on its own,
// and the recorder is bounded and in memory.
const cspReportPath = "/ui/csp-report"

const maxCSPReportBytes = 8 * 1024

// pageWriter carries the per-request script nonce and the tracking snippet
// from the policy middleware to render, so the policy header and the markup
// agree without threading the request through every handler.
type pageWriter struct {
	http.ResponseWriter
	nonce   string
	snippet template.HTML
	head    bool
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *pageWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// secure sets the content security policy on every UI response. Pages get a
// script policy that allows the application origin and a fresh nonce; the
// tracking snippet, when active for the path, is rendered with that nonce and
// its origins are added. 'unsafe-inline' is never used for scripts, so turning
// tracking off narrows the policy back to exactly what it was.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		path := r.URL.Path
		if path == cspReportPath || strings.HasPrefix(path, tracking.ProxyPath+"/") || strings.HasPrefix(path, "/ui/static/") ||
			path == "/favicon.ico" || path == "/favicon.png" || path == "/logo.png" || path == "/ui/jobs/status" {
			// Not a page: nothing runs here, so nothing is allowed.
			w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			next.ServeHTTP(w, r)
			return
		}
		nonce := newNonce()
		config := s.app.TrackingConfig(r.Context())
		pw := &pageWriter{ResponseWriter: w, nonce: nonce}
		if config.Active(path) {
			pw.snippet = template.HTML(config.Snippet(nonce)) // #nosec G203 -- administrator-authored snippet, nonce-bound
			pw.head = config.Placement != "body"
		}
		w.Header().Set("Content-Security-Policy", pagePolicy(config, path, nonce))
		next.ServeHTTP(pw, r)
	})
}

// pagePolicy assembles the policy for one page. Styles allow inline values
// because the templates and sanitised mail bodies carry style attributes;
// scripts never do — the layout's own scripts carry the nonce too.
func pagePolicy(config tracking.Config, path, nonce string) string {
	scripts := []string{"'self'", "'nonce-" + nonce + "'"}
	connects := []string{"'self'"}
	images := []string{"'self'", "data:"}
	active := config.Active(path)
	if active {
		extraScripts, extraConnects, extraImages := config.PolicySources()
		scripts = append(scripts, extraScripts...)
		connects = append(connects, extraConnects...)
		images = append(images, extraImages...)
	}
	policy := "default-src 'self'; script-src " + strings.Join(scripts, " ") +
		"; style-src 'self' 'unsafe-inline'; img-src " + strings.Join(images, " ") +
		"; connect-src " + strings.Join(connects, " ") +
		"; object-src 'none'; frame-ancestors 'none'; base-uri 'self'"
	if active {
		// While tracking is on, ask the browser to say what it refused. That
		// report is what turns a console error into a one-click fix.
		policy += "; report-uri " + cspReportPath
	}
	return policy
}

func newNonce() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// cspReport is the report-uri payload (the legacy shape every browser still
// sends for report-uri).
type cspReport struct {
	Report struct {
		BlockedURI         string `json:"blocked-uri"`
		EffectiveDirective string `json:"effective-directive"`
		ViolatedDirective  string `json:"violated-directive"`
		DocumentURI        string `json:"document-uri"`
	} `json:"csp-report"`
}

func (s *Server) receiveCSPReport(w http.ResponseWriter, r *http.Request) {
	defer w.WriteHeader(http.StatusNoContent)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCSPReportBytes))
	if err != nil || len(body) == 0 {
		return
	}
	var report cspReport
	if json.Unmarshal(body, &report) != nil {
		return
	}
	directive := report.Report.EffectiveDirective
	if directive == "" {
		directive = report.Report.ViolatedDirective
	}
	s.violations.Record(report.Report.BlockedURI, directive, pageOf(report.Report.DocumentURI))
}

// pageOf keeps only the path of the reporting document; the origin is ours
// and the query may hold a search term.
func pageOf(documentURI string) string {
	parsed, err := url.Parse(documentURI)
	if err != nil {
		return ""
	}
	return parsed.Path
}

func (s *Server) adminTrackingAllow(w http.ResponseWriter, r *http.Request) {
	if err := s.app.AdminAllowTrackingHost(r.Context(), r.FormValue("origin")); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/admin/settings?saved=1#tracking", http.StatusSeeOther)
}

func (s *Server) adminTrackingForget(w http.ResponseWriter, r *http.Request) {
	if p, ok := application.PrincipalFrom(r.Context()); !ok || !p.IsAdmin() {
		s.render(w, "error", http.StatusForbidden, map[string]any{"Message": "관리자 권한이 필요합니다."})
		return
	}
	s.violations.Forget()
	http.Redirect(w, r, "/ui/admin/settings#tracking", http.StatusSeeOther)
}

// momentoProxy forwards /momento/* to the collector so the browser only ever
// talks to this origin. It is live only while Momento is the active provider
// with the proxy switched on; otherwise the path does not exist.
func (s *Server) momentoProxy(w http.ResponseWriter, r *http.Request) {
	config := s.app.TrackingConfig(r.Context())
	if !config.ProxyEnabled() {
		http.NotFound(w, r)
		return
	}
	target, err := url.Parse(strings.TrimRight(strings.TrimSpace(config.MomentoURL), "/"))
	if err != nil || target.Host == "" {
		http.NotFound(w, r)
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + strings.TrimPrefix(r.URL.Path, tracking.ProxyPath)
			pr.Out.URL.RawPath = ""
			pr.SetXForwarded()
			// The collector gets visitor events, never the session.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("momento proxy failed", "err", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}
