package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"postra/internal/application"
	"postra/internal/domain"
)

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func forwardedFirst(value string) string {
	value, _, _ = strings.Cut(value, ",")
	return strings.TrimSpace(value)
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil || strings.EqualFold(forwardedFirst(r.Header.Get("X-Forwarded-Proto")), "https") {
		return "https"
	}
	return "http"
}

// Cookie mutations require the full same origin (scheme, host, and port),
// not merely the same hostname/site. Reverse proxies must overwrite their
// forwarded headers and preserve the public Host or X-Forwarded-Host.
func sameOrigin(r *http.Request, required bool) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return !required
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	host := r.Host
	if forwarded := forwardedFirst(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}
	return strings.EqualFold(u.Scheme, requestScheme(r)) && strings.EqualFold(u.Host, host)
}

func (s *Server) localPrincipal(r *http.Request) (domain.Principal, bool) {
	u, err := s.app.Store.GetUser(r.Context(), application.DefaultUserID)
	if err != nil || u.Status != domain.UserActive {
		return domain.Principal{}, false
	}
	return domain.Principal{UserID: u.ID, LoginID: u.LoginID, DisplayName: u.DisplayName, Role: domain.RoleAdmin, AuthMethod: "local"}, true
}

func (s *Server) browserMutationAllowed(r *http.Request) bool {
	if safeMethod(r.Method) {
		return true
	}
	if !sameOrigin(r, true) {
		return false
	}
	c, err := r.Cookie("postra_session")
	if err != nil {
		return false
	}
	csrf := r.Header.Get("X-CSRF-Token")
	if session, _, err := s.app.AuthenticateSession(r.Context(), c.Value); err == nil {
		return s.app.VerifyCSRF(session, csrf)
	}
	// Legacy single-user token-cookie login has no server-side session. A
	// separate random double-submit cookie plus exact Origin protects it.
	if !s.app.Cfg.Auth.Enabled && s.apiToken != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.apiToken)) == 1 {
		tokenCookie, err := r.Cookie("postra_csrf")
		return err == nil && csrf != "" && subtle.ConstantTimeCompare([]byte(csrf), []byte(tokenCookie.Value)) == 1
	}
	return false
}

func (s *Server) browserSession(w http.ResponseWriter, r *http.Request) {
	response := map[string]any{
		"authenticated": false,
		"auth_enabled":  s.app.Cfg.Auth.Enabled || s.apiToken != "",
		"login_url":     "/ui/login?return_to=%2Fapp%2F",
	}
	if s.app.Cfg.Auth.Enabled {
		if needs, err := s.app.NeedsAdminSetup(r.Context()); err == nil && needs {
			response["setup_url"] = "/ui/setup"
		}
		if s.app.OIDCConfigured(r.Context()) {
			response["oidc_url"] = "/ui/auth/oidc/start?return_to=%2Fapp%2F"
			response["oidc_auto_login"] = s.app.OIDCAutoLoginEnabled(r.Context())
		}
	}
	p, ok := s.authenticate(r)
	if !s.app.Cfg.Auth.Enabled && s.apiToken == "" {
		p, ok = s.localPrincipal(r)
	}
	if ok {
		response["authenticated"] = true
		response["principal"] = p
		// Populate only the non-secret anti-CSRF cookie for old token-cookie
		// logins. Session and API credentials never appear in JSON or JS.
		if !s.app.Cfg.Auth.Enabled && s.apiToken != "" && r.Header.Get("Authorization") == "" {
			if _, err := r.Cookie("postra_csrf"); err != nil {
				var entropy [24]byte
				if _, err := rand.Read(entropy[:]); err == nil {
					http.SetCookie(w, &http.Cookie{Name: "postra_csrf", Value: base64.RawURLEncoding.EncodeToString(entropy[:]), Path: "/", Secure: requestScheme(r) == "https", SameSite: http.SameSiteLaxMode}) // #nosec G124 -- readable CSRF cookie; Secure follows deployed transport
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) browserLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("postra_session"); err == nil {
		if session, _, err := s.app.AuthenticateSession(r.Context(), c.Value); err == nil {
			if err := s.app.Logout(r.Context(), session.ID); err != nil {
				writeErr(w, err)
				return
			}
		}
	}
	for _, name := range []string{"postra_session", "postra_csrf"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: name == "postra_session", Secure: requestScheme(r) == "https", SameSite: http.SameSiteLaxMode}) // #nosec G124 -- deletion mirrors dynamically secure shared cookies
	}
	writeJSON(w, http.StatusOK, map[string]string{"login_url": "/ui/login?sso=signed_out&return_to=%2Fapp%2F"})
}
