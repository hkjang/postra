package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"postra/internal/application"
)

const browserOIDCErrorCookie = "postra_oidc_error"

func (s *Server) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/session", s.browserSession)
	mux.HandleFunc("POST /auth/login", s.authLogin)
	mux.HandleFunc("GET /auth/setup", s.authSetupState)
	mux.HandleFunc("POST /auth/setup", s.authSetup)
	mux.HandleFunc("POST /auth/logout", s.browserLogout)
	mux.HandleFunc("GET /auth/oidc/start", s.authOIDCStart)
	mux.HandleFunc("GET /auth/oidc/callback", s.authOIDCCallback)
}

func publicAuthPath(path string) bool {
	switch path {
	case "/auth/session", "/auth/login", "/auth/setup", "/auth/oidc/start", "/auth/oidc/callback":
		return true
	}
	return false
}

// Unauthenticated login/bootstrap cannot use an existing session CSRF token.
// Require an exact browser origin plus JSON so another site cannot force a
// login or install its account through a cross-origin HTML form submission.
func decodeAuthRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	if !sameOrigin(r, true) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "동일 출처에서 다시 요청하세요."})
		return false
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "JSON 요청이 필요합니다."})
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "로그인 요청 형식이 올바르지 않습니다."})
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "하나의 JSON 요청만 허용됩니다."})
		return false
	}
	return true
}

func browserReturnTo(raw string) string {
	if !application.SafeReturnTo(raw) || strings.ContainsAny(raw, "\\\x00") {
		return "/app/"
	}
	u, err := url.Parse(raw)
	if err != nil || !(u.Path == "/app" || strings.HasPrefix(u.Path, "/app/")) ||
		u.Path == "/app/login" || u.Path == "/app/setup" || u.Path == "/app/error" {
		return "/app/"
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == ".." || segment == "." {
			return "/app/"
		}
	}
	return raw
}

func (s *Server) setBrowserCookies(w http.ResponseWriter, r *http.Request, session, csrf string) {
	for _, cookie := range []*http.Cookie{
		{Name: "postra_session", Value: session, Path: "/", HttpOnly: true, Secure: requestScheme(r) == "https", SameSite: http.SameSiteLaxMode}, // #nosec G124 -- Secure is mandatory on HTTPS; offline HTTP deployments cannot accept a Secure cookie
		{Name: "postra_csrf", Value: csrf, Path: "/", Secure: requestScheme(r) == "https", SameSite: http.SameSiteLaxMode},                       // #nosec G124 -- intentionally readable CSRF cookie, never the session credential
	} {
		http.SetCookie(w, cookie) // #nosec G124 -- HttpOnly session and separate readable CSRF; Secure follows deployed transport
	}
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LoginID  string `json:"login_id"`
		Password string `json:"password"`
		Token    string `json:"token"`
		ReturnTo string `json:"return_to"`
	}
	if !decodeAuthRequest(w, r, &in) {
		return
	}
	if s.app.Cfg.Auth.Enabled {
		u, err := s.app.AuthenticateLocal(application.WithActor(r.Context(), "browser"), in.LoginID, in.Password)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "로그인 ID 또는 비밀번호가 올바르지 않습니다."})
			return
		}
		raw, csrf, _, err := s.app.CreateSession(r.Context(), u, r.UserAgent(), application.ClientIP(r.RemoteAddr))
		if err != nil {
			writeErr(w, err)
			return
		}
		s.setBrowserCookies(w, r, raw, csrf)
	} else if s.apiToken != "" {
		if subtle.ConstantTimeCompare([]byte(in.Token), []byte(s.apiToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "토큰이 올바르지 않습니다."})
			return
		}
		var entropy [24]byte
		if _, err := rand.Read(entropy[:]); err != nil {
			writeErr(w, err)
			return
		}
		s.setBrowserCookies(w, r, s.apiToken, base64.RawURLEncoding.EncodeToString(entropy[:]))
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect_url": browserReturnTo(in.ReturnTo)})
}

func setupAllowed(r *http.Request, app *application.App) bool {
	ip := net.ParseIP(application.ClientIP(r.RemoteAddr))
	return (ip != nil && ip.IsLoopback()) || app.Cfg.Auth.BootstrapPassword != ""
}

func (s *Server) authSetupState(w http.ResponseWriter, r *http.Request) {
	required := false
	if s.app.Cfg.Auth.Enabled {
		var err error
		required, err = s.app.NeedsAdminSetup(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"required": required, "allowed": required && setupAllowed(r, s.app), "login_url": "/app/login"})
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LoginID     string `json:"login_id"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if !decodeAuthRequest(w, r, &in) {
		return
	}
	if !s.app.Cfg.Auth.Enabled {
		http.NotFound(w, r)
		return
	}
	needs, err := s.app.NeedsAdminSetup(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if !needs {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "최초 관리자 설정이 이미 완료되었습니다."})
		return
	}
	if !setupAllowed(r, s.app) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "원격 초기 설정은 POSTRA_BOOTSTRAP_ADMIN_PASSWORD 환경변수가 필요합니다."})
		return
	}
	u, err := s.app.SetupInitialAdmin(application.WithActor(r.Context(), "setup"), in.LoginID, in.DisplayName, in.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	raw, csrf, _, err := s.app.CreateSession(r.Context(), u, r.UserAgent(), application.ClientIP(r.RemoteAddr))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.setBrowserCookies(w, r, raw, csrf)
	writeJSON(w, http.StatusCreated, map[string]string{"redirect_url": "/app/admin/settings"})
}

func authLoginMarker(marker, returnTo string) string {
	q := url.Values{"sso": {marker}, "return_to": {browserReturnTo(returnTo)}}
	return "/app/login?" + q.Encode()
}

func (s *Server) clearBrowserOIDCFlow(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "postra_oidc_flow", Value: "", Path: "/auth", MaxAge: -1, HttpOnly: true, Secure: requestScheme(r) == "https", SameSite: http.SameSiteLaxMode}) // #nosec G124 -- deletion follows original transport security
}

func (s *Server) failBrowserOIDC(w http.ResponseWriter, r *http.Request, returnTo, message string) {
	s.clearBrowserOIDCFlow(w, r)
	if note, err := s.app.SignOIDCError(message); err == nil {
		http.SetCookie(w, &http.Cookie{Name: browserOIDCErrorCookie, Value: note, Path: "/auth/session", MaxAge: int(application.OIDCErrorNoteTTL.Seconds()), HttpOnly: true, Secure: requestScheme(r) == "https", SameSite: http.SameSiteLaxMode}) // #nosec G124 -- signed one-shot note, HttpOnly with transport-matched Secure
	}
	http.Redirect(w, r, authLoginMarker("error", returnTo), http.StatusFound)
}

func (s *Server) consumeBrowserOIDCError(w http.ResponseWriter, r *http.Request) string {
	message := "SSO 로그인이 실패했습니다. 다시 시도하세요."
	if cookie, err := r.Cookie(browserOIDCErrorCookie); err == nil {
		if verified, ok := s.app.VerifyOIDCError(cookie.Value); ok {
			message = verified
		}
		http.SetCookie(w, &http.Cookie{Name: browserOIDCErrorCookie, Value: "", Path: "/auth/session", MaxAge: -1, HttpOnly: true, Secure: requestScheme(r) == "https", SameSite: http.SameSiteLaxMode}) // #nosec G124 -- deletion mirrors the signed note's scope
	}
	return message
}

func (s *Server) authOIDCStart(w http.ResponseWriter, r *http.Request) {
	if !s.app.Cfg.Auth.Enabled {
		http.NotFound(w, r)
		return
	}
	returnTo := browserReturnTo(r.URL.Query().Get("return_to"))
	authURL, flow, err := s.app.BeginOIDC(r.Context(), application.OIDCStartOptions{Silent: r.URL.Query().Get("prompt") == "none", ReturnTo: returnTo})
	if err != nil {
		s.failBrowserOIDC(w, r, returnTo, err.Error())
		return
	}
	signed, err := s.app.SignOIDCFlow(flow)
	if err != nil {
		s.failBrowserOIDC(w, r, returnTo, "SSO 로그인 요청을 시작하지 못했습니다.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "postra_oidc_flow", Value: signed, Path: "/auth", MaxAge: 600, HttpOnly: true, Secure: requestScheme(r) == "https", SameSite: http.SameSiteLaxMode}) // #nosec G124 -- signed flow, HttpOnly and transport-matched Secure
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

func (s *Server) authOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if !s.app.Cfg.Auth.Enabled {
		http.NotFound(w, r)
		return
	}
	var flow application.OIDCFlow
	verified := false
	if cookie, err := r.Cookie("postra_oidc_flow"); err == nil {
		if candidate, err := s.app.VerifyOIDCFlow(cookie.Value); err == nil && subtle.ConstantTimeCompare([]byte(candidate.State), []byte(r.URL.Query().Get("state"))) == 1 {
			flow, verified = candidate, true
		}
	}
	if !verified {
		s.failBrowserOIDC(w, r, "", "OIDC 로그인 상태가 없거나 state 검증에 실패했습니다. 다시 로그인하세요.")
		return
	}
	if providerError := r.URL.Query().Get("error"); providerError != "" {
		if flow.Silent && application.OIDCLoginRequired(providerError) {
			s.clearBrowserOIDCFlow(w, r)
			http.Redirect(w, r, authLoginMarker("none", flow.ReturnTo), http.StatusFound)
			return
		}
		message := "Keycloak 로그인이 실패했습니다: " + application.OIDCProviderErrorGuidance(providerError)
		s.failBrowserOIDC(w, r, flow.ReturnTo, message)
		return
	}
	u, err := s.app.CompleteOIDC(application.WithActor(r.Context(), "oidc"), r.URL.Query().Get("code"), flow)
	if err != nil {
		s.failBrowserOIDC(w, r, flow.ReturnTo, err.Error())
		return
	}
	raw, csrf, _, err := s.app.CreateSession(r.Context(), u, r.UserAgent(), application.ClientIP(r.RemoteAddr))
	if err != nil {
		s.failBrowserOIDC(w, r, flow.ReturnTo, "로그인 세션을 만들지 못했습니다.")
		return
	}
	s.clearBrowserOIDCFlow(w, r)
	s.setBrowserCookies(w, r, raw, csrf)
	http.Redirect(w, r, browserReturnTo(flow.ReturnTo), http.StatusSeeOther)
}
