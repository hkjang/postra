// Package webui is a minimal, server-rendered web UI (§ mail-web): mail search,
// draft review, and the two-step send-approval flow. It is intentionally
// dependency-free — Go html/template + embedded assets, no CDN, no build step —
// so it ships in the single binary and works on air-gapped networks. Handlers
// are thin and call the same application.App the REST and MCP transports use.
package webui

import (
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
)

//go:embed templates/*.html
var files embed.FS

// cookieName carries the API token for UI sessions. SameSite=Strict is the
// CSRF baseline for the state-changing (send) forms.
const cookieName = "postra_ui"

type Server struct {
	app      *application.App
	apiToken string
	tpl      map[string]*template.Template
}

func New(app *application.App, apiToken string) *Server {
	s := &Server{app: app, apiToken: apiToken}
	s.tpl = parseTemplates()
	return s
}

var funcs = template.FuncMap{
	"addr":  func(a domain.Address) string { return formatAddr(a) },
	"addrs": func(as []domain.Address) string { return formatAddrs(as) },
	"join":  func(ss []string) string { return strings.Join(ss, ", ") },
	"ts": func(unix int64) string {
		if unix == 0 {
			return "—"
		}
		return time.Unix(unix, 0).Format("2006-01-02 15:04")
	},
	"size": humanSize,
}

// parseTemplates builds one template set per page (layout + page), so each
// page's {{define "content"}} stays isolated.
func parseTemplates() map[string]*template.Template {
	pages := []string{"search", "message", "draft", "send", "sent", "login", "error"}
	out := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		t := template.Must(template.New("layout").Funcs(funcs).
			ParseFS(files, "templates/layout.html", "templates/"+p+".html"))
		out[p] = t
	}
	return out
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ui/login", s.loginForm)
	mux.HandleFunc("POST /ui/login", s.loginSubmit)
	mux.HandleFunc("GET /ui/", s.gate(s.search))
	mux.HandleFunc("GET /ui/messages/{id}", s.gate(s.message))
	mux.HandleFunc("GET /ui/drafts/{id}", s.gate(s.draft))
	mux.HandleFunc("GET /ui/drafts/{id}/send", s.gate(s.sendForm))
	mux.HandleFunc("POST /ui/drafts/{id}/send", s.gate(s.sendSubmit))
	return mux
}

// gate enforces the cookie login when an API token is configured. With no
// token (offline default) the UI is open, matching the REST API's posture.
func (s *Server) gate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken != "" && !s.authed(r) {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		h(w, r.WithContext(application.WithActor(r.Context(), "webui")))
	}
}

func (s *Server) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.apiToken)) == 1
}

// ---------- handlers ----------

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if s.apiToken == "" {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", http.StatusOK, map[string]any{})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if s.apiToken == "" {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(s.apiToken)) != 1 {
		s.render(w, "login", http.StatusUnauthorized, map[string]any{"Error": "토큰이 올바르지 않습니다."})
		return
	}
	// #nosec G124 -- Secure is intentionally omitted: the server serves plain
	// HTTP for offline/air-gapped use, where a Secure cookie would never be
	// sent and login would break. HttpOnly + SameSite=Strict are the baseline;
	// terminate TLS at a reverse proxy when exposing to untrusted networks.
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.apiToken, Path: "/ui/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

const searchPageSize = 50

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cursor := r.URL.Query().Get("cursor")
	data := map[string]any{"Query": q, "Searched": r.URL.Query().Has("q")}
	if q != "" {
		res, err := s.app.Search(r.Context(), domain.SearchQuery{
			UserID: application.DefaultUserID, Text: q, Limit: searchPageSize, Cursor: cursor,
		})
		if err != nil {
			s.fail(w, err)
			return
		}
		data["Messages"] = res.Messages
		if res.NextCursor != "" {
			// Preserve the query and advance the cursor for the "more" link.
			data["NextURL"] = "/ui/?q=" + url.QueryEscape(q) + "&cursor=" + url.QueryEscape(res.NextCursor)
		}
	}
	s.render(w, "search", http.StatusOK, data)
}

func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	view, err := s.app.GetMessage(r.Context(), r.PathValue("id"), true)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "message", http.StatusOK, map[string]any{"View": view})
}

func (s *Server) draft(w http.ResponseWriter, r *http.Request) {
	view, err := s.app.GetDraft(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "draft", http.StatusOK, map[string]any{"View": view})
}

func (s *Server) sendForm(w http.ResponseWriter, r *http.Request) {
	preview, err := s.app.PreviewSend(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "send", http.StatusOK, map[string]any{"Preview": preview})
}

func (s *Server) sendSubmit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.FormValue("action") {
	case "approve":
		preview, tok, err := s.app.RequestSendApproval(r.Context(), id, "webui", 900)
		if err != nil {
			s.fail(w, err)
			return
		}
		s.render(w, "send", http.StatusOK, map[string]any{"Preview": preview, "Token": tok.Token})
	case "confirm":
		out, err := s.app.Send(r.Context(), application.SendInput{
			DraftID: id, ApprovalToken: r.FormValue("token"),
		})
		if err != nil {
			s.fail(w, err)
			return
		}
		s.render(w, "sent", http.StatusOK, map[string]any{"Outbound": out})
	default:
		http.Redirect(w, r, "/ui/drafts/"+id+"/send", http.StatusSeeOther)
	}
}

// ---------- render helpers ----------

func (s *Server) render(w http.ResponseWriter, page string, code int, data map[string]any) {
	t, ok := s.tpl[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("webui render failed", "page", page, "err", err)
	}
}

// fail renders user errors as 400 and everything else as 500, mirroring the
// REST error mapping so the UI never leaks raw internals as a 200.
func (s *Server) fail(w http.ResponseWriter, err error) {
	var ue *application.UserError
	code, msg := http.StatusInternalServerError, "요청을 처리할 수 없습니다."
	switch {
	case errors.As(err, &ue):
		code, msg = http.StatusBadRequest, ue.Msg
	case errors.Is(err, domain.ErrNotFound):
		code, msg = http.StatusNotFound, "찾을 수 없습니다."
	default:
		slog.Error("webui request failed", "err", err)
	}
	s.render(w, "error", code, map[string]any{"Message": msg})
}

func formatAddr(a domain.Address) string {
	if a.Name != "" {
		return a.Name + " <" + a.Email + ">"
	}
	return a.Email
}

func formatAddrs(as []domain.Address) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		parts = append(parts, formatAddr(a))
	}
	return strings.Join(parts, ", ")
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGT"[exp]) + "B"
}
