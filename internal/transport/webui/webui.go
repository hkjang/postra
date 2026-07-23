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
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"postra/internal/application"
	"postra/internal/domain"
	"postra/internal/platform/metrics"
)

//go:embed templates/*.html
var files embed.FS

const (
	cookieName = "postra_session"
	csrfCookie = "postra_csrf"
)

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
	pages := []string{"search", "message", "draft", "send", "sent", "login", "error",
		"accounts", "account_new", "account", "compose", "analysis", "job",
		"setup", "admin_users", "admin_settings"}
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
	mux.HandleFunc("GET /ui/setup", s.setupForm)
	mux.HandleFunc("POST /ui/setup", s.setupSubmit)
	mux.HandleFunc("GET /ui/login", s.loginForm)
	mux.HandleFunc("POST /ui/login", s.loginSubmit)
	mux.HandleFunc("GET /ui/auth/oidc/start", s.oidcStart)
	mux.HandleFunc("GET /ui/auth/oidc/callback", s.oidcCallback)
	mux.HandleFunc("POST /ui/logout", s.gate(s.logout))
	mux.HandleFunc("GET /ui/admin/users", s.gate(s.adminUsers))
	mux.HandleFunc("POST /ui/admin/users", s.gate(s.adminUserCreate))
	mux.HandleFunc("POST /ui/admin/users/{id}", s.gate(s.adminUserUpdate))
	mux.HandleFunc("POST /ui/admin/users/{id}/password", s.gate(s.adminPasswordReset))
	mux.HandleFunc("GET /ui/admin/settings", s.gate(s.adminSettings))
	mux.HandleFunc("POST /ui/admin/settings", s.gate(s.adminSettingsSave))
	mux.HandleFunc("GET /ui/", s.gate(s.search))
	mux.HandleFunc("GET /ui/accounts", s.gate(s.accounts))
	mux.HandleFunc("GET /ui/accounts/new", s.gate(s.accountNew))
	mux.HandleFunc("POST /ui/accounts", s.gate(s.accountCreate))
	mux.HandleFunc("GET /ui/accounts/{id}", s.gate(s.account))
	mux.HandleFunc("POST /ui/accounts/{id}/test", s.gate(s.accountTest))
	mux.HandleFunc("POST /ui/accounts/{id}/sync", s.gate(s.accountSync))
	mux.HandleFunc("GET /ui/jobs/{id}", s.gate(s.job))
	mux.HandleFunc("GET /ui/compose", s.gate(s.composeForm))
	mux.HandleFunc("POST /ui/compose", s.gate(s.composeSubmit))
	mux.HandleFunc("GET /ui/messages/{id}", s.gate(s.message))
	mux.HandleFunc("GET /ui/messages/{id}/attachments/{att}", s.gate(s.attachment))
	mux.HandleFunc("POST /ui/messages/{id}/analyze", s.gate(s.analyze))
	mux.HandleFunc("POST /ui/messages/{id}/draft", s.gate(s.messageDraft))
	mux.HandleFunc("GET /ui/drafts/{id}", s.gate(s.draft))
	mux.HandleFunc("POST /ui/drafts/{id}", s.gate(s.draftUpdate))
	mux.HandleFunc("POST /ui/drafts/{id}/rewrite", s.gate(s.draftRewrite))
	mux.HandleFunc("GET /ui/drafts/{id}/send", s.gate(s.sendForm))
	mux.HandleFunc("POST /ui/drafts/{id}/send", s.gate(s.sendSubmit))
	return mux
}

// gate enforces the cookie login when an API token is configured. With no
// token (offline default) the UI is open, matching the REST API's posture.
func (s *Server) gate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.app.Cfg.Auth.Enabled {
			needsSetup, err := s.app.NeedsAdminSetup(r.Context())
			if err != nil {
				s.fail(w, err)
				return
			}
			if needsSetup {
				http.Redirect(w, r, "/ui/setup", http.StatusSeeOther)
				return
			}
			c, err := r.Cookie(cookieName)
			if err != nil {
				http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
				return
			}
			_, principal, err := s.app.AuthenticateSession(r.Context(), c.Value)
			if err != nil {
				s.clearAuthCookies(w, r)
				http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if !validRequestOrigin(r) {
					s.render(w, "error", http.StatusForbidden, map[string]any{"Message": "요청 검증에 실패했습니다. 페이지를 새로고침하세요."})
					return
				}
			}
			ctx := application.WithPrincipal(application.WithActor(r.Context(), "webui"), principal)
			h(w, r.WithContext(ctx))
			return
		}
		if s.apiToken != "" && !s.authed(r) {
			http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
			return
		}
		h(w, r.WithContext(application.WithActor(r.Context(), "webui")))
	}
}

func validRequestOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser clients; SameSite=Lax still protects the session cookie
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	requestHost := r.Host
	if forwarded := firstForwarded(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		requestHost = forwarded
	}
	return strings.EqualFold(u.Host, requestHost)
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
	w.Header().Set("Cache-Control", "no-store")
	if s.app.Cfg.Auth.Enabled {
		if needs, _ := s.app.NeedsAdminSetup(r.Context()); needs {
			http.Redirect(w, r, "/ui/setup", http.StatusSeeOther)
			return
		}
		s.render(w, "login", http.StatusOK, map[string]any{"LocalAuth": true, "OIDCEnabled": s.app.OIDCConfigured(r.Context())})
		return
	}
	if s.apiToken == "" {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", http.StatusOK, map[string]any{})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.app.Cfg.Auth.Enabled {
		u, err := s.app.AuthenticateLocal(application.WithActor(r.Context(), "webui"), r.FormValue("login_id"), r.FormValue("password"))
		if err != nil {
			s.render(w, "login", http.StatusUnauthorized, map[string]any{
				"Error": err.Error(), "LocalAuth": true, "OIDCEnabled": s.app.OIDCConfigured(r.Context()),
			})
			return
		}
		raw, csrf, _, err := s.app.CreateSession(r.Context(), u, r.UserAgent(), application.ClientIP(r.RemoteAddr))
		if err != nil {
			s.fail(w, err)
			return
		}
		s.setAuthCookies(w, r, raw, csrf)
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
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
		Name: cookieName, Value: s.apiToken, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	if !s.app.Cfg.Auth.Enabled {
		http.NotFound(w, r)
		return
	}
	authURL, flow, err := s.app.BeginOIDC(r.Context())
	if err != nil {
		s.render(w, "login", http.StatusBadGateway, map[string]any{
			"Error": err.Error(), "LocalAuth": true, "OIDCEnabled": true,
		})
		return
	}
	signed, err := s.app.SignOIDCFlow(flow)
	if err != nil {
		s.fail(w, err)
		return
	}
	// #nosec G124 -- Secure is enabled for TLS/proxied HTTPS; loopback HTTP is intentionally supported.
	http.SetCookie(w, &http.Cookie{Name: "postra_oidc_flow", Value: signed, Path: "/",
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if oidcErr := r.URL.Query().Get("error"); oidcErr != "" {
		s.render(w, "login", http.StatusUnauthorized, map[string]any{
			"Error":     "Keycloak 로그인이 취소되었거나 실패했습니다: " + oidcErr,
			"LocalAuth": true, "OIDCEnabled": true,
		})
		return
	}
	c, err := r.Cookie("postra_oidc_flow")
	if err != nil {
		s.render(w, "login", http.StatusBadRequest, map[string]any{"Error": "OIDC 로그인 상태가 없습니다.", "LocalAuth": true, "OIDCEnabled": true})
		return
	}
	flow, err := s.app.VerifyOIDCFlow(c.Value)
	if err != nil || subtle.ConstantTimeCompare([]byte(flow.State), []byte(r.URL.Query().Get("state"))) != 1 {
		s.render(w, "login", http.StatusBadRequest, map[string]any{"Error": "OIDC state 검증에 실패했습니다.", "LocalAuth": true, "OIDCEnabled": true})
		return
	}
	u, err := s.app.CompleteOIDC(application.WithActor(r.Context(), "oidc"), r.URL.Query().Get("code"), flow)
	if err != nil {
		s.render(w, "login", http.StatusUnauthorized, map[string]any{"Error": err.Error(), "LocalAuth": true, "OIDCEnabled": true})
		return
	}
	raw, csrf, _, err := s.app.CreateSession(r.Context(), u, r.UserAgent(), application.ClientIP(r.RemoteAddr))
	if err != nil {
		s.fail(w, err)
		return
	}
	// #nosec G124 -- Secure follows the transport so loopback HTTP remains usable.
	http.SetCookie(w, &http.Cookie{Name: "postra_oidc_flow", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	s.setAuthCookies(w, r, raw, csrf)
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(firstForwarded(r.Header.Get("X-Forwarded-Proto")), "https")
}

func firstForwarded(value string) string {
	if value, _, ok := strings.Cut(value, ","); ok {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(value)
}

func (s *Server) setAuthCookies(w http.ResponseWriter, r *http.Request, session, csrf string) {
	secure := secureRequest(r)
	// #nosec G124 -- Secure is enabled for TLS/proxied HTTPS; loopback HTTP is intentionally supported.
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: session, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	// #nosec G124 -- readable anti-CSRF cookie; SameSite and dynamic Secure are explicitly set.
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrf, Path: "/",
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode})
}

func (s *Server) clearAuthCookies(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{cookieName, csrfCookie} {
		// #nosec G124 -- deletion cookie mirrors the dynamically secure original cookie.
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: name == cookieName, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	}
}

func (s *Server) setupForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.app.Cfg.Auth.Enabled {
		http.Redirect(w, r, "/ui/", http.StatusSeeOther)
		return
	}
	needs, err := s.app.NeedsAdminSetup(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if !needs {
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
		return
	}
	s.render(w, "setup", http.StatusOK, map[string]any{})
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.app.Cfg.Auth.Enabled {
		http.NotFound(w, r)
		return
	}
	needs, err := s.app.NeedsAdminSetup(r.Context())
	if err != nil || !needs {
		http.Error(w, "setup unavailable", http.StatusForbidden)
		return
	}
	if !isLoopbackRequest(r) && s.app.Cfg.Auth.BootstrapPassword == "" {
		s.render(w, "setup", http.StatusForbidden, map[string]any{
			"Error": "원격 초기 설정은 POSTRA_BOOTSTRAP_ADMIN_PASSWORD 환경변수가 필요합니다.",
		})
		return
	}
	u, err := s.app.SetupInitialAdmin(application.WithActor(r.Context(), "setup"),
		r.FormValue("login_id"), r.FormValue("display_name"), r.FormValue("password"))
	if err != nil {
		s.render(w, "setup", http.StatusBadRequest, map[string]any{"Error": err.Error()})
		return
	}
	raw, csrf, _, err := s.app.CreateSession(r.Context(), u, r.UserAgent(), application.ClientIP(r.RemoteAddr))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setAuthCookies(w, r, raw, csrf)
	http.Redirect(w, r, "/ui/admin/settings", http.StatusSeeOther)
}

func isLoopbackRequest(r *http.Request) bool {
	ip := net.ParseIP(application.ClientIP(r.RemoteAddr))
	return ip != nil && ip.IsLoopback()
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		if ss, _, err := s.app.AuthenticateSession(r.Context(), c.Value); err == nil {
			_ = s.app.Logout(r.Context(), ss.ID)
		}
	}
	s.clearAuthCookies(w, r)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func csrfFromRequest(r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.app.AdminListUsers(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "admin_users", http.StatusOK, map[string]any{"Users": users, "CSRF": csrfFromRequest(r)})
}

func (s *Server) adminUserCreate(w http.ResponseWriter, r *http.Request) {
	_, err := s.app.AdminCreateUser(r.Context(), application.CreateUserInput{
		LoginID: r.FormValue("login_id"), DisplayName: r.FormValue("display_name"),
		Email: r.FormValue("email"), Role: domain.UserRole(r.FormValue("role")), Password: r.FormValue("password"),
	})
	if err != nil {
		users, _ := s.app.AdminListUsers(r.Context())
		s.render(w, "admin_users", http.StatusBadRequest, map[string]any{"Users": users, "CSRF": csrfFromRequest(r), "Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/ui/admin/users", http.StatusSeeOther)
}

func (s *Server) adminUserUpdate(w http.ResponseWriter, r *http.Request) {
	if _, err := s.app.AdminUpdateUser(r.Context(), r.PathValue("id"), domain.UserRole(r.FormValue("role")),
		domain.UserStatus(r.FormValue("status"))); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/admin/users", http.StatusSeeOther)
}

func (s *Server) adminPasswordReset(w http.ResponseWriter, r *http.Request) {
	if err := s.app.AdminResetPassword(r.Context(), r.PathValue("id"), r.FormValue("password")); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/admin/users", http.StatusSeeOther)
}

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	if p, ok := application.PrincipalFrom(r.Context()); !ok || !p.IsAdmin() {
		s.render(w, "error", http.StatusForbidden, map[string]any{"Message": "관리자 권한이 필요합니다."})
		return
	}
	settings, err := s.app.SystemSettings(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "admin_settings", http.StatusOK, map[string]any{"Settings": settings})
}

func (s *Server) adminSettingsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	values := map[string]string{}
	for key := range r.Form {
		if strings.HasPrefix(key, "auth.") || strings.HasPrefix(key, "sync.") ||
			strings.HasPrefix(key, "ai.") || strings.HasPrefix(key, "send.") ||
			strings.HasPrefix(key, "security.") || strings.HasPrefix(key, "attachments.") {
			values[key] = r.FormValue(key)
		}
	}
	// Unchecked checkboxes are absent from form encoding.
	for _, key := range []string{application.SettingOIDCAutoProvision, application.SettingAIAllowExternal,
		application.SettingAIMaskExternalPII, application.SettingAllowInsecureMail,
		application.SettingAllowPrivateHosts, application.SettingEncryptAtRest} {
		if _, ok := values[key]; !ok {
			values[key] = "false"
		}
	}
	if err := s.app.AdminSaveSettings(r.Context(), values, r.FormValue("oidc_client_secret")); err != nil {
		settings, _ := s.app.SystemSettings(r.Context())
		s.render(w, "admin_settings", http.StatusBadRequest, map[string]any{"Settings": settings, "Error": err.Error()})
		return
	}
	http.Redirect(w, r, "/ui/admin/settings?saved=1", http.StatusSeeOther)
}

const searchPageSize = 50

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cursor := r.URL.Query().Get("cursor")
	accountID := r.URL.Query().Get("account")
	accounts, err := s.app.ListAccounts(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.app.Search(r.Context(), domain.SearchQuery{
		UserID: application.DefaultUserID, Text: q, AccountID: accountID,
		Limit: searchPageSize, Cursor: cursor,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	data := map[string]any{
		"Query": q, "Messages": res.Messages, "Accounts": accounts,
		"AccountID": accountID, "HasAccounts": len(accounts) > 0,
		"HasMessages": len(res.Messages) > 0,
	}
	if res.NextCursor != "" {
		next := "/ui/?q=" + url.QueryEscape(q)
		if accountID != "" {
			next += "&account=" + url.QueryEscape(accountID)
		}
		data["NextURL"] = next + "&cursor=" + url.QueryEscape(res.NextCursor)
	}
	s.render(w, "search", http.StatusOK, data)
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.app.ListAccounts(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "accounts", http.StatusOK, map[string]any{"Accounts": accounts})
}

func (s *Server) accountNew(w http.ResponseWriter, r *http.Request) {
	s.render(w, "account_new", http.StatusOK, map[string]any{})
}

func intForm(r *http.Request, name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
	return n
}

func splitAddresses(s string) []string {
	var out []string
	for _, value := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (s *Server) accountCreate(w http.ResponseWriter, r *http.Request) {
	password := r.FormValue("password")
	if password == "" {
		s.render(w, "account_new", http.StatusBadRequest, map[string]any{
			"Error": "메일 비밀번호를 입력하세요.", "Form": r.Form,
		})
		return
	}
	label := strings.TrimSpace(r.FormValue("name"))
	if label == "" {
		label = strings.TrimSpace(r.FormValue("email"))
	}
	inRef, err := s.app.RegisterSecret(r.Context(), domain.SecretMailPassword, label+" 수신",
		domain.NewSecretHandle([]byte(password)))
	if err != nil {
		s.fail(w, err)
		return
	}
	smtpRef := inRef
	if smtpPassword := r.FormValue("smtp_password"); smtpPassword != "" && smtpPassword != password {
		smtpRef, err = s.app.RegisterSecret(r.Context(), domain.SecretMailPassword, label+" 발신",
			domain.NewSecretHandle([]byte(smtpPassword)))
		if err != nil {
			_ = s.app.RevokeSecret(r.Context(), inRef)
			s.fail(w, err)
			return
		}
	}
	acc, err := s.app.CreateAccount(r.Context(), application.CreateAccountInput{
		Name: label, Email: strings.TrimSpace(r.FormValue("email")),
		InboundProtocol: r.FormValue("inbound_protocol"),
		POP3Host:        strings.TrimSpace(r.FormValue("inbound_host")),
		POP3Port:        intForm(r, "inbound_port"), POP3Security: r.FormValue("inbound_security"),
		POP3Username: strings.TrimSpace(r.FormValue("inbound_username")), POP3SecretRef: string(inRef),
		SMTPHost: strings.TrimSpace(r.FormValue("smtp_host")), SMTPPort: intForm(r, "smtp_port"),
		SMTPSecurity: r.FormValue("smtp_security"), SMTPUsername: strings.TrimSpace(r.FormValue("smtp_username")),
		SMTPAuth: r.FormValue("smtp_auth"), SMTPSecretRef: string(smtpRef),
	})
	if err != nil {
		metrics.UIActions.WithLabelValues("account_create", "error").Inc()
		_ = s.app.RevokeSecret(r.Context(), inRef)
		if smtpRef != inRef {
			_ = s.app.RevokeSecret(r.Context(), smtpRef)
		}
		s.render(w, "account_new", http.StatusBadRequest, map[string]any{"Error": err.Error(), "Form": r.Form})
		return
	}
	metrics.UIActions.WithLabelValues("account_create", "ok").Inc()
	http.Redirect(w, r, "/ui/accounts/"+acc.ID+"?created=1", http.StatusSeeOther)
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	acc, err := s.app.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "account", http.StatusOK, map[string]any{
		"Account": acc, "Created": r.URL.Query().Get("created") == "1",
	})
}

func (s *Server) accountTest(w http.ResponseWriter, r *http.Request) {
	acc, err := s.app.GetAccount(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	diags, err := s.app.TestAccount(r.Context(), acc.ID)
	if err != nil {
		metrics.UIActions.WithLabelValues("account_test", "error").Inc()
		s.render(w, "account", http.StatusBadRequest, map[string]any{"Account": acc, "Error": err.Error()})
		return
	}
	result := "ok"
	for _, diag := range diags {
		if !diag.OK {
			result = "failed"
			break
		}
	}
	metrics.UIActions.WithLabelValues("account_test", result).Inc()
	s.render(w, "account", http.StatusOK, map[string]any{"Account": acc, "Diagnostics": diags, "Tested": true})
}

func (s *Server) accountSync(w http.ResponseWriter, r *http.Request) {
	job, err := s.app.StartSync(r.Context(), r.PathValue("id"), application.SyncOptions{})
	if err != nil {
		metrics.UIActions.WithLabelValues("sync_start", "error").Inc()
		acc, getErr := s.app.GetAccount(r.Context(), r.PathValue("id"))
		if getErr != nil {
			s.fail(w, err)
			return
		}
		s.render(w, "account", http.StatusBadRequest, map[string]any{"Account": acc, "Error": err.Error()})
		return
	}
	metrics.UIActions.WithLabelValues("sync_start", "ok").Inc()
	http.Redirect(w, r, "/ui/jobs/"+job.ID, http.StatusSeeOther)
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	job, err := s.app.GetJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	running := job.Status == domain.JobQueued || job.Status == domain.JobRunning
	s.render(w, "job", http.StatusOK, map[string]any{"Job": job, "Running": running})
}

func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	view, err := s.app.GetMessage(r.Context(), r.PathValue("id"), true)
	if err != nil {
		s.fail(w, err)
		return
	}
	accounts, _ := s.app.ListAccounts(r.Context())
	s.render(w, "message", http.StatusOK, map[string]any{"View": view, "Accounts": accounts})
}

func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	ack := r.URL.Query().Get("ack") == "true"
	at, rc, err := s.app.GetAttachment(r.Context(), r.PathValue("id"), r.PathValue("att"), ack)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", at.MIMEType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": at.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, rc); err != nil {
		slog.Error("webui attachment stream failed", "err", err)
	}
}

func (s *Server) analyze(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("type")
	switch kind {
	case "summarize", "phishing", "action_items":
	default:
		s.render(w, "error", http.StatusBadRequest, map[string]any{"Message": "지원하지 않는 AI 분석 유형입니다."})
		return
	}
	an, err := s.app.AnalyzeMessage(r.Context(), r.PathValue("id"), kind)
	if err != nil {
		metrics.UIActions.WithLabelValues("ai_analysis", "error").Inc()
		s.render(w, "analysis", http.StatusBadRequest, map[string]any{
			"Error": err.Error(), "MessageID": r.PathValue("id"),
		})
		return
	}
	metrics.UIActions.WithLabelValues("ai_analysis", "ok").Inc()
	s.render(w, "analysis", http.StatusOK, map[string]any{"Analysis": an, "MessageID": r.PathValue("id")})
}

func (s *Server) messageDraft(w http.ResponseWriter, r *http.Request) {
	mv, err := s.app.GetMessage(r.Context(), r.PathValue("id"), false)
	if err != nil {
		metrics.UIActions.WithLabelValues("reply_draft_create", "error").Inc()
		s.fail(w, err)
		return
	}
	metrics.UIActions.WithLabelValues("reply_draft_create", "ok").Inc()
	kind := r.FormValue("kind")
	if kind == "" {
		kind = "reply"
	}
	dv, err := s.app.CreateDraft(r.Context(), application.CreateDraftInput{
		AccountID: mv.Message.AccountID, Kind: kind, ReplyToMessageID: mv.Message.ID,
		Instructions: strings.TrimSpace(r.FormValue("instructions")),
	})
	if err != nil {
		metrics.UIActions.WithLabelValues("new_draft_create", "error").Inc()
		s.fail(w, err)
		return
	}
	metrics.UIActions.WithLabelValues("new_draft_create", "ok").Inc()
	http.Redirect(w, r, "/ui/drafts/"+dv.Draft.ID, http.StatusSeeOther)
}

func (s *Server) composeForm(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.app.ListAccounts(r.Context())
	if err != nil {
		metrics.UIActions.WithLabelValues("draft_update", "error").Inc()
		s.fail(w, err)
		return
	}
	metrics.UIActions.WithLabelValues("draft_update", "ok").Inc()
	s.render(w, "compose", http.StatusOK, map[string]any{"Accounts": accounts})
}

func (s *Server) composeSubmit(w http.ResponseWriter, r *http.Request) {
	dv, err := s.app.CreateDraft(r.Context(), application.CreateDraftInput{
		AccountID: r.FormValue("account_id"), Kind: "new",
		To: splitAddresses(r.FormValue("to")), Cc: splitAddresses(r.FormValue("cc")),
		Subject: r.FormValue("subject"), Body: r.FormValue("body"),
		Instructions: strings.TrimSpace(r.FormValue("instructions")),
	})
	if err != nil {
		accounts, _ := s.app.ListAccounts(r.Context())
		s.render(w, "compose", http.StatusBadRequest, map[string]any{
			"Accounts": accounts, "Error": err.Error(), "Form": r.Form,
		})
		return
	}
	http.Redirect(w, r, "/ui/drafts/"+dv.Draft.ID, http.StatusSeeOther)
}

func (s *Server) draft(w http.ResponseWriter, r *http.Request) {
	view, err := s.app.GetDraft(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "draft", http.StatusOK, map[string]any{"View": view})
}

func (s *Server) draftUpdate(w http.ResponseWriter, r *http.Request) {
	subject, body := r.FormValue("subject"), r.FormValue("body")
	_, err := s.app.UpdateDraft(r.Context(), application.UpdateDraftInput{
		DraftID: r.PathValue("id"), Subject: &subject, Body: &body,
		To: splitAddresses(r.FormValue("to")), Cc: splitAddresses(r.FormValue("cc")),
		Bcc: splitAddresses(r.FormValue("bcc")),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/drafts/"+r.PathValue("id")+"?saved=1", http.StatusSeeOther)
}

func (s *Server) draftRewrite(w http.ResponseWriter, r *http.Request) {
	if _, err := s.app.RewriteDraft(r.Context(), r.PathValue("id"), r.FormValue("style")); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui/drafts/"+r.PathValue("id"), http.StatusSeeOther)
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
			metrics.UIActions.WithLabelValues("send_approval", "error").Inc()
			s.fail(w, err)
			return
		}
		metrics.UIActions.WithLabelValues("send_approval", "ok").Inc()
		s.render(w, "send", http.StatusOK, map[string]any{"Preview": preview, "Token": tok.Token})
	case "confirm":
		out, err := s.app.Send(r.Context(), application.SendInput{
			DraftID: id, ApprovalToken: r.FormValue("token"),
		})
		if err != nil {
			metrics.UIActions.WithLabelValues("send_confirm", "error").Inc()
			s.fail(w, err)
			return
		}
		metrics.UIActions.WithLabelValues("send_confirm", "ok").Inc()
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
	if page == "login" || page == "setup" {
		data["AuthPage"] = true
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
